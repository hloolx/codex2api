package imagestore

import (
	"container/list"
	"os"
	"strconv"
	"sync"
)

// ThumbnailCache 是一个非常小的进程内 LRU，用来缓存按 (assetID, thumbKB) 生成的缩略图。
//
// 之所以引入它：本地后端读盘成本可忽略，但 S3 后端每次读源图都要走一次外网请求，
// 缩略图重生成也要 CPU。LRU 命中之后直接 c.Data() 返回，零回源。
//
// 设计取舍：
//   - 不持久化（重启清零，数据足够小）
//   - 不在 key 里编码 mime；不同后端/格式重新生成时按 capBytes 自然淘汰旧条目
//   - 64MB 默认上限，按字节淘汰；超大单图（>capBytes）拒绝缓存
type ThumbnailCache struct {
	mu       sync.Mutex
	ll       *list.List
	idx      map[string]*list.Element
	capBytes int64
	curBytes int64
}

type thumbEntry struct {
	key         string
	contentType string
	data        []byte
}

// NewThumbnailCache 创建容量为 capBytes 字节的 LRU。capBytes <= 0 时使用 64MB 默认值。
func NewThumbnailCache(capBytes int64) *ThumbnailCache {
	if capBytes <= 0 {
		capBytes = 64 * 1024 * 1024
		if value, err := strconv.ParseInt(os.Getenv("IMAGE_THUMB_CACHE_MB"), 10, 64); err == nil && value >= 1 && value <= 1024 {
			capBytes = value * 1024 * 1024
		}
	}
	return &ThumbnailCache{
		ll:       list.New(),
		idx:      make(map[string]*list.Element),
		capBytes: capBytes,
	}
}

// ThumbKey 拼出缓存 key。
func ThumbKey(assetID int64, thumbKB int) string {
	return strconv.FormatInt(assetID, 10) + ":" + strconv.Itoa(thumbKB)
}

// Get 命中时返回 (data, contentType, true)。
func (c *ThumbnailCache) Get(key string) ([]byte, string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.idx[key]
	if !ok {
		return nil, "", false
	}
	c.ll.MoveToFront(el)
	en := el.Value.(*thumbEntry)
	return en.data, en.contentType, true
}

// Put 写入。data 大于 capBytes 时静默忽略。
func (c *ThumbnailCache) Put(key, contentType string, data []byte) {
	if len(data) == 0 || int64(len(data)) > c.capBytes {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	if el, ok := c.idx[key]; ok {
		en := el.Value.(*thumbEntry)
		c.curBytes -= int64(len(en.data))
		en.contentType = contentType
		en.data = data
		c.curBytes += int64(len(data))
		c.ll.MoveToFront(el)
	} else {
		en := &thumbEntry{key: key, contentType: contentType, data: data}
		el := c.ll.PushFront(en)
		c.idx[key] = el
		c.curBytes += int64(len(data))
	}

	for c.curBytes > c.capBytes {
		oldest := c.ll.Back()
		if oldest == nil {
			break
		}
		en := oldest.Value.(*thumbEntry)
		c.ll.Remove(oldest)
		delete(c.idx, en.key)
		c.curBytes -= int64(len(en.data))
	}
}

// Invalidate 在删除资源时清掉所有 thumbKB 变体。
func (c *ThumbnailCache) Invalidate(assetID int64) {
	prefix := strconv.FormatInt(assetID, 10) + ":"
	c.mu.Lock()
	defer c.mu.Unlock()
	for key, el := range c.idx {
		if !startsWith(key, prefix) {
			continue
		}
		en := el.Value.(*thumbEntry)
		c.ll.Remove(el)
		delete(c.idx, key)
		c.curBytes -= int64(len(en.data))
	}
}

func startsWith(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}
