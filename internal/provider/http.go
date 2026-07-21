package provider

import (
	"context"
	"fmt"
	"io"
	"net/http"
)

func readLimited(response *http.Response, limit int64, code string) ([]byte, error) {
	if response.ContentLength > limit {
		return nil, fail(code, "provider response is too large", true)
	}
	reader := io.LimitReader(response.Body, limit+1)
	body, err := io.ReadAll(reader)
	if err != nil {
		return nil, wrap(code, "read provider response", err, true)
	}
	if int64(len(body)) > limit {
		return nil, fail(code, "provider response is too large", true)
	}
	return body, nil
}

func newRequest(ctx context.Context, method, url string, body io.Reader) (*http.Request, error) {
	request, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	request.Header.Set("Accept", "application/json")
	return request, nil
}
