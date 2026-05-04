package repo

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"sync/atomic"
)

// secret_crypto.go 提供静态包级 AES-256-GCM 加解密：
//
// 唯一对外 API：
//
//	SetDataKey(hex)  —— 启动期一次（cmd/* 从 cfg.DataKey 装载）
//
// 内部实现给 AuthConfig.Scanner/Valuer 用——其它 typed JSON（Routing/Quota/Capabilities）
// 不加密（非敏感）。未来需要给更多列加密时也走这套 encryptBlob/decryptBlob。
//
// **格式**：blobPrefix + base64( nonce || ciphertext || tag )
// blobPrefix 用 "v1:"，未来 KEK 轮转可加 "v2:"。
//
// **未设置 KEK**：encrypt/decrypt 返回错误（不 panic）；调用栈一直冒到
// startup CheckSchema → fail-fast。

const blobPrefix = "v1:"

// aeadAtomic 持有 cipher.AEAD 的指针，SetDataKey 后被 publish。
// 用 atomic.Pointer 而不是 sync.Mutex —— hot path 是 read-mostly。
var aeadAtomic atomic.Pointer[cipher.AEAD]

// SetDataKey 用 hex-encoded 32 字节 key 初始化 AES-256-GCM cipher。
//
//	hexKey 必须正好 64 个 hex 字符（= 256 bit）；其它长度直接报错。
//
// 重复调用会替换旧的 cipher（生产正常不会发生，测试可能反复 set）。
func SetDataKey(hexKey string) error {
	if len(hexKey) != 64 {
		return fmt.Errorf("repo: data_key must be 64 hex chars (got %d)", len(hexKey))
	}
	key, err := hex.DecodeString(hexKey)
	if err != nil {
		return fmt.Errorf("repo: data_key hex decode: %w", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return fmt.Errorf("repo: aes.NewCipher: %w", err)
	}
	a, err := cipher.NewGCM(block)
	if err != nil {
		return fmt.Errorf("repo: cipher.NewGCM: %w", err)
	}
	aeadAtomic.Store(&a)
	return nil
}

// encryptBlob 加密 plain → "v1:" + base64(nonce||ct||tag)。
//
// 每次新随机 nonce；可以反复调用同一份 plain 得到不同 ciphertext。
func encryptBlob(plain []byte) ([]byte, error) {
	a, err := loadAEAD()
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, a.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("repo: rand nonce: %w", err)
	}
	ct := a.Seal(nil, nonce, plain, nil)
	raw := make([]byte, 0, len(nonce)+len(ct))
	raw = append(raw, nonce...)
	raw = append(raw, ct...)
	encoded := base64.StdEncoding.EncodeToString(raw)
	return []byte(blobPrefix + encoded), nil
}

// decryptBlob 还原 plain。
func decryptBlob(b []byte) ([]byte, error) {
	a, err := loadAEAD()
	if err != nil {
		return nil, err
	}
	if !bytes.HasPrefix(b, []byte(blobPrefix)) {
		return nil, fmt.Errorf("repo: blob missing %q prefix", blobPrefix)
	}
	encoded := b[len(blobPrefix):]
	raw, err := base64.StdEncoding.DecodeString(string(encoded))
	if err != nil {
		return nil, fmt.Errorf("repo: blob base64: %w", err)
	}
	if len(raw) < a.NonceSize() {
		return nil, errors.New("repo: blob too short for nonce")
	}
	nonce, ct := raw[:a.NonceSize()], raw[a.NonceSize():]
	plain, err := a.Open(nil, nonce, ct, nil)
	if err != nil {
		return nil, fmt.Errorf("repo: aead open: %w", err)
	}
	return plain, nil
}

// loadAEAD 取当前 cipher.AEAD；nil 时报错（KEK 未装载）。
func loadAEAD() (cipher.AEAD, error) {
	p := aeadAtomic.Load()
	if p == nil {
		return nil, errors.New("repo: data_key not set; call repo.SetDataKey at startup")
	}
	return *p, nil
}
