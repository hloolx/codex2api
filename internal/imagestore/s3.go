package imagestore

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// S3Backend 适配任意 S3-compatible 对象存储：AWS S3、Cloudflare R2、MinIO、阿里云 OSS、
// 腾讯云 COS、Backblaze B2、华为 OBS、七牛 Kodo 等。
//
// ref 形态固定为 "s3://<bucket>/<key>"。bucket 与配置一致；key 含 prefix。
type S3Backend struct {
	client *s3.Client
	bucket string
	prefix string
}

// NewS3Backend 按 cfg 构造客户端。Region 留空时默认 "auto"（兼容 R2）。
//
// 兼容性处理：
//   - SwapComputePayloadSHA256ForUnsignedPayloadMiddleware：避免对 PUT 体计算 SHA256，
//     绕开阿里云 OSS / 腾讯云 COS 等服务的签名差异。
//   - RequestChecksumCalculationWhenRequired：仅在协议要求时计算校验和，
//     避免新版 SDK 默认开启的 CRC64 与旧 S3 实现不兼容。
//   - UsePathStyle：MinIO、部分自建 / 早期 S3 服务必需。
func NewS3Backend(cfg Config) (*S3Backend, error) {
	cfg = cfg.Normalize()
	if cfg.Bucket == "" {
		return nil, fmt.Errorf("imagestore.s3: 缺少 bucket")
	}

	region := cfg.Region
	if region == "" {
		region = "auto"
	}

	awsCfg, err := awsconfig.LoadDefaultConfig(context.Background(),
		awsconfig.WithRegion(region),
		awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(cfg.AccessKey, cfg.SecretKey, ""),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("加载 AWS 配置失败: %w", err)
	}

	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		if cfg.Endpoint != "" {
			endpoint := cfg.Endpoint
			o.BaseEndpoint = &endpoint
		}
		if cfg.ForcePathStyle {
			o.UsePathStyle = true
		}
		o.APIOptions = append(o.APIOptions, v4.SwapComputePayloadSHA256ForUnsignedPayloadMiddleware)
		o.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
	})

	return &S3Backend{client: client, bucket: cfg.Bucket, prefix: cfg.Prefix}, nil
}

// Name 实现 Backend。
func (b *S3Backend) Name() string { return BackendS3 }

// Save 上传一个对象。返回 "s3://bucket/key" 形态的 ref。
func (b *S3Backend) Save(ctx context.Context, key string, data []byte, mime string) (string, error) {
	if strings.TrimSpace(key) == "" {
		return "", fmt.Errorf("imagestore.s3: key 为空")
	}
	fullKey := b.prefix + key
	contentType := mime
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	_, err := b.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      &b.bucket,
		Key:         &fullKey,
		Body:        bytes.NewReader(data),
		ContentType: &contentType,
	})
	if err != nil {
		return "", fmt.Errorf("S3 PutObject: %w", err)
	}
	return buildS3Ref(b.bucket, fullKey), nil
}

// Open 拉取对象内容流。size 来自 Content-Length（若可用）。
func (b *S3Backend) Open(ctx context.Context, ref string) (io.ReadCloser, int64, error) {
	bucket, key, err := parseS3Ref(ref)
	if err != nil {
		return nil, 0, err
	}
	if bucket == "" {
		bucket = b.bucket
	}
	out, err := b.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: &bucket,
		Key:    &key,
	})
	if err != nil {
		return nil, 0, fmt.Errorf("S3 GetObject: %w", err)
	}
	var size int64 = -1
	if out.ContentLength != nil {
		size = *out.ContentLength
	}
	return out.Body, size, nil
}

// Read 一次性读出全部内容。
func (b *S3Backend) Read(ctx context.Context, ref string) ([]byte, error) {
	rc, _, err := b.Open(ctx, ref)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

// Delete 删除对象。
func (b *S3Backend) Delete(ctx context.Context, ref string) error {
	bucket, key, err := parseS3Ref(ref)
	if err != nil {
		return err
	}
	if bucket == "" {
		bucket = b.bucket
	}
	_, err = b.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: &bucket,
		Key:    &key,
	})
	if err != nil {
		return fmt.Errorf("S3 DeleteObject: %w", err)
	}
	return nil
}

// HeadBucket 提供探活：用于设置页"测试连接"按钮。
func (b *S3Backend) HeadBucket(ctx context.Context) error {
	_, err := b.client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: &b.bucket})
	if err != nil {
		return fmt.Errorf("S3 HeadBucket: %w", err)
	}
	return nil
}

// PresignGetURL 为一个 ref 生成带过期时间的预签名 GET 直链。
//
// 签名在本地完成、不发起网络请求，因此可在无网环境下单测。适配私有桶：
// 拿到链接的一方可在 ttl 内直接 GET 下载，无需公开读权限。
func (b *S3Backend) PresignGetURL(ctx context.Context, ref string, ttl time.Duration) (string, error) {
	bucket, key, err := parseS3Ref(ref)
	if err != nil {
		return "", err
	}
	if bucket == "" {
		bucket = b.bucket
	}
	if ttl <= 0 {
		ttl = 15 * time.Minute
	}
	presign := s3.NewPresignClient(b.client)
	out, err := presign.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket: &bucket,
		Key:    &key,
	}, s3.WithPresignExpires(ttl))
	if err != nil {
		return "", fmt.Errorf("S3 PresignGetObject: %w", err)
	}
	return out.URL, nil
}

func buildS3Ref(bucket, key string) string {
	return s3RefScheme + bucket + "/" + key
}

func parseS3Ref(ref string) (bucket, key string, err error) {
	ref = strings.TrimSpace(ref)
	if !strings.HasPrefix(ref, s3RefScheme) {
		return "", "", fmt.Errorf("imagestore.s3: 非 S3 ref: %s", ref)
	}
	rest := strings.TrimPrefix(ref, s3RefScheme)
	idx := strings.IndexByte(rest, '/')
	if idx <= 0 || idx == len(rest)-1 {
		return "", "", fmt.Errorf("imagestore.s3: 非法 ref: %s", ref)
	}
	return rest[:idx], rest[idx+1:], nil
}

// Compile-time interface check.
var _ Backend = (*S3Backend)(nil)

// SaveReader uploads an encoded image without allocating another image buffer.
func (b *S3Backend) SaveReader(ctx context.Context, key string, source io.ReadSeeker, size int64, mime string) (string, error) {
	if strings.TrimSpace(key) == "" {
		return "", fmt.Errorf("empty image key")
	}
	fullKey := b.prefix + key
	_, err := b.client.PutObject(ctx, &s3.PutObjectInput{Bucket: &b.bucket, Key: &fullKey, Body: source, ContentType: &mime, ContentLength: &size})
	if err != nil {
		return "", err
	}
	return buildS3Ref(b.bucket, fullKey), nil
}
