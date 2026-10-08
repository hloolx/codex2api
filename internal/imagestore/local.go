package imagestore

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// LocalBackend 把图片写到本地文件系统。ref 即文件绝对路径，与历史行为完全一致。
type LocalBackend struct {
	dir string
}

// NewLocalBackend 创建本地后端。dir 为空表示由调用方决定（实际写入时由调用方拼接绝对路径）。
//
// 我们仍然在这里持有一个 dir，是为了让 admin 侧的 path-allowlist 校验能从一处取到边界。
func NewLocalBackend(dir string) (*LocalBackend, error) {
	dir = strings.TrimSpace(dir)
	if dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("创建图片目录失败: %w", err)
		}
	}
	return &LocalBackend{dir: dir}, nil
}

// Name 实现 Backend。
func (b *LocalBackend) Name() string { return BackendLocal }

// Dir 返回根目录。
func (b *LocalBackend) Dir() string { return b.dir }

// Save 把字节流写入 dir/key（key 是文件名）。返回绝对路径作为 ref。
func (b *LocalBackend) Save(_ context.Context, key string, data []byte, _ string) (string, error) {
	if strings.TrimSpace(key) == "" {
		return "", fmt.Errorf("imagestore.local: key 为空")
	}
	if b.dir == "" {
		return "", fmt.Errorf("imagestore.local: 未设置目录")
	}
	name := filepath.Base(key)
	if name != key || name == "." || name == ".." || strings.ContainsAny(key, `/\`) {
		return "", fmt.Errorf("imagestore.local: key 不能包含路径分隔符: %q", key)
	}
	full := filepath.Join(b.dir, name)
	if err := os.WriteFile(full, data, 0o644); err != nil {
		return "", err
	}
	abs, err := filepath.Abs(full)
	if err != nil {
		return full, nil
	}
	return abs, nil
}

// validateLocalRef 做读/删侧的纵深防御：ref 由 Save 产出并保存在数据库里，正常数据
// 不会含 ".." 段（Join/Abs 都会 Clean 掉）。这里只拦截遍历形式，不强制绝对路径也不
// 强制落在当前 dir 内——LocalDir 是运行时设置，历史资源可能写在旧目录，且重构前的
// 旧版本在 IMAGE_ASSET_DIR 为相对路径时会把相对 ref 存进数据库。
func validateLocalRef(ref string) error {
	if slices.Contains(strings.Split(filepath.ToSlash(ref), "/"), "..") {
		return fmt.Errorf("imagestore.local: 非法 ref: %q", ref)
	}
	return nil
}

// Open 流式读取本地文件。
func (b *LocalBackend) Open(_ context.Context, ref string) (io.ReadCloser, int64, error) {
	if ref == "" {
		return nil, 0, os.ErrNotExist
	}
	if err := validateLocalRef(ref); err != nil {
		return nil, 0, err
	}
	f, err := os.Open(ref)
	if err != nil {
		return nil, 0, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, 0, err
	}
	return f, info.Size(), nil
}

// Read 一次性读出，便于做缩略图。
func (b *LocalBackend) Read(_ context.Context, ref string) ([]byte, error) {
	if ref == "" {
		return nil, os.ErrNotExist
	}
	if err := validateLocalRef(ref); err != nil {
		return nil, err
	}
	return os.ReadFile(ref)
}

// Delete 删除文件，文件不存在视为成功。
func (b *LocalBackend) Delete(_ context.Context, ref string) error {
	if ref == "" {
		return nil
	}
	if err := validateLocalRef(ref); err != nil {
		return err
	}
	if err := os.Remove(ref); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// Compile-time interface check.
var _ Backend = (*LocalBackend)(nil)

// helper：方便 Save 之外的位置构造 reader/buffer。
var _ = bytes.NewReader

// SaveReader copies an already encoded image without loading it into memory.
func (b *LocalBackend) SaveReader(ctx context.Context, key string, source io.ReadSeeker, size int64, mime string) (string, error) {
	if b.dir == "" || key == "" || filepath.Base(key) != key || key == "." || key == ".." || strings.ContainsAny(key, `/\`) {
		return "", fmt.Errorf("invalid local image key")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(b.dir, ".image-write-")
	if err != nil {
		return "", err
	}
	name := f.Name()
	defer os.Remove(name)
	n, err := io.Copy(f, source)
	closeErr := f.Close()
	if err != nil {
		return "", err
	}
	if closeErr != nil {
		return "", closeErr
	}
	if size >= 0 && n != size {
		return "", io.ErrUnexpectedEOF
	}
	if err = ctx.Err(); err != nil {
		return "", err
	}
	full := filepath.Join(b.dir, key)
	if err = os.Rename(name, full); err != nil {
		return "", err
	}
	return filepath.Abs(full)
}
