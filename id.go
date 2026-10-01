// id.go —— ID 生成（对齐 rust-store/host/src/id.rs 与 nodejs-store/src/crud/id.js）：
// `prefix + 毫秒base36大写 + 8位随机base36`。core 无时钟无随机源（铁律），
// now 与 new_id 一律由宿主供给。
package gostore

import (
	"fmt"
	"sync"
	"time"
)

const base36Chars = "0123456789abcdefghijklmnopqrstuvwxyz"

func toBase36(n uint64) string {
	if n == 0 {
		return "0"
	}
	buf := make([]byte, 0, 16)
	for n > 0 {
		buf = append(buf, base36Chars[n%36])
		n /= 36
	}
	// 反转
	for i, j := 0, len(buf)-1; i < j; i, j = i+1, j-1 {
		buf[i], buf[j] = buf[j], buf[i]
	}
	return string(buf)
}

// xorshift64 状态（宿主侧非密码学用途；种子取纳秒时钟熵）
type xorshift struct {
	mu sync.Mutex
	s  uint64
}

func (x *xorshift) next() uint64 {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.s ^= x.s << 13
	x.s ^= x.s >> 7
	x.s ^= x.s << 17
	return x.s
}

func (x *xorshift) rand8() string {
	out := make([]byte, 8)
	for i := range out {
		out[i] = base36Chars[x.next()%36]
	}
	return string(out)
}

func nowMS() int64 {
	return time.Now().UnixMilli()
}

// generateID 生成一个 schema ID（对齐 id.rs generate_id）。
func (x *xorshift) generateID(prefix string) string {
	return fmt.Sprintf("%s%s%s", prefix, toBase36(uint64(nowMS())), x.rand8())
}
