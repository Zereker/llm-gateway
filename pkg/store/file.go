package store

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// FileKV 是 KV 的零依赖默认实现：
// 一个目录 = 一个 KV；每个 key 对应 root 下的一个 .json 文件。
//
// 例如 root = "/etc/ai-gateway"，则：
//
//	key "modelservice/svc_gpt4o" → /etc/ai-gateway/modelservice/svc_gpt4o.json
//	key "ratelimit/user/alice/svc_gpt4o" → /etc/ai-gateway/ratelimit/user/alice/svc_gpt4o.json
//
// 适合本地开发 / 小规模部署 + 手动编辑文件 + 重启网关生效的场景。
//
// **v0.1 不支持 Watch 推送**（Watch 返回的 channel 永不发事件）；
// 数据变更需重启网关。生产热加载应使用 etcd / fsnotify 实现（v0.5+）。
type FileKV struct {
	root string
}

// NewFileKV 用 root 目录构造。目录不存在会自动创建。
func NewFileKV(root string) (*FileKV, error) {
	if root == "" {
		return nil, errors.New("store: FileKV root is empty")
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	return &FileKV{root: root}, nil
}

// Get 实现 KV.Get。
func (s *FileKV) Get(_ context.Context, key string) (json.RawMessage, error) {
	data, err := os.ReadFile(s.pathOf(key))
	if err != nil {
		return nil, err
	}
	return json.RawMessage(data), nil
}

// List 实现 KV.List：遍历 root/<prefix> 下所有文件，key 用相对路径。
//
// prefix 为空时遍历整个 root。
func (s *FileKV) List(_ context.Context, prefix string) (map[string]json.RawMessage, error) {
	out := map[string]json.RawMessage{}
	base := filepath.Join(s.root, prefix)
	err := filepath.WalkDir(base, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if errors.Is(walkErr, fs.ErrNotExist) {
				return filepath.SkipAll
			}
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(s.root, path)
		key := strings.TrimSuffix(strings.ReplaceAll(rel, string(os.PathSeparator), "/"), ".json")
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		out[key] = json.RawMessage(data)
		return nil
	})
	if errors.Is(err, fs.ErrNotExist) {
		return out, nil
	}
	return out, err
}

// Watch 实现 KV.Watch。
//
// **v0.1：返回不发任何事件的 channel**；ctx cancel 时 close channel。
// 调用方仍可正常 select，只是不会收到变更（等同"必须重启网关"）。
func (s *FileKV) Watch(c context.Context, _ string) (<-chan Event, error) {
	ch := make(chan Event)
	go func() {
		<-c.Done()
		close(ch)
	}()
	return ch, nil
}

// Put 实现 KV.Put。
func (s *FileKV) Put(_ context.Context, key string, value json.RawMessage) error {
	p := s.pathOf(key)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, value, 0o644)
}

// Delete 实现 KV.Delete；不存在视为成功（幂等）。
func (s *FileKV) Delete(_ context.Context, key string) error {
	err := os.Remove(s.pathOf(key))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

func (s *FileKV) pathOf(key string) string {
	return filepath.Join(s.root, key+".json")
}

// 编译期断言。
var _ KV = (*FileKV)(nil)
