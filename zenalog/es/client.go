package es

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"time"
)

// defaultTimeout 单次 ES 请求的默认超时
const defaultTimeout = 10 * time.Second

// client 裸 REST 客户端：依次尝试各节点，网络错误换下一个，HTTP 应答原样返回
type client struct {
	addrs    []string
	username string
	password string
	hc       *http.Client
}

func newClient(cfg Config) *client {
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	return &client{
		addrs:    cfg.Addresses,
		username: cfg.Username,
		password: cfg.Password,
		hc:       &http.Client{Timeout: timeout},
	}
}

// do 发一次请求。网络层失败逐节点重试，收到 HTTP 应答（无论状态码）即返回，
// 状态码语义由调用方判定。
func (c *client) do(ctx context.Context, method, path string, body []byte, contentType string) (int, []byte, error) {
	var lastErr error
	for _, addr := range c.addrs {
		req, err := http.NewRequestWithContext(ctx, method, addr+path, bytes.NewReader(body))
		if err != nil {
			return 0, nil, fmt.Errorf("es: build request %s %s: %w", method, path, err)
		}
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		if c.username != "" {
			req.SetBasicAuth(c.username, c.password)
		}
		resp, err := c.hc.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		respBody, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			return 0, nil, fmt.Errorf("es: read response %s %s: %w", method, path, err)
		}
		return resp.StatusCode, respBody, nil
	}
	return 0, nil, fmt.Errorf("es: all %d nodes unreachable for %s %s: %w", len(c.addrs), method, path, lastErr)
}

// snippet 截断响应体，避免错误信息带上超长 body
func snippet(body []byte) string {
	const max = 512
	if len(body) > max {
		return string(body[:max]) + "..."
	}
	return string(body)
}
