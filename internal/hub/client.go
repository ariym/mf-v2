package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"runtime"
	"strconv"
	"strings"
	"time"
)

type Client struct {
	URL   string
	Token string
	HTTP  *http.Client
}
type Page struct {
	Items  []json.RawMessage `json:"items"`
	Limit  int               `json:"limit"`
	Offset int               `json:"offset"`
}

func (c Client) request(ctx context.Context, method, path string, body []byte, out any) error {
	u, e := url.Parse(strings.TrimRight(c.URL, "/") + path)
	if e != nil {
		return e
	}
	if u.User != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return fmt.Errorf("hub URL must use HTTP(S) without credentials")
	}
	if method == http.MethodPost && c.Token != "" && u.Scheme != "https" && u.Hostname() != "localhost" && u.Hostname() != "127.0.0.1" && u.Hostname() != "::1" {
		return fmt.Errorf("hub credentials require HTTPS outside localhost")
	}
	req, e := http.NewRequestWithContext(ctx, method, u.String(), bytes.NewReader(body))
	if e != nil {
		return e
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if method == http.MethodPost && c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	client := c.HTTP
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	// A publication must never be redirected to another host or change method.
	clone := *client
	clone.CheckRedirect = func(req *http.Request, via []*http.Request) error { return http.ErrUseLastResponse }
	resp, e := clone.Do(req)
	if e != nil {
		return e
	}
	defer resp.Body.Close()
	raw, e := io.ReadAll(io.LimitReader(resp.Body, 16<<20+1))
	if e != nil {
		return e
	}
	if len(raw) > 16<<20 {
		return fmt.Errorf("hub response exceeds 16 MiB")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var v struct {
			Error struct{ Code, Message string }
		}
		_ = json.Unmarshal(raw, &v)
		return fmt.Errorf("hub HTTP %d %s: %s", resp.StatusCode, v.Error.Code, v.Error.Message)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(raw, out)
}
func (c Client) Health(ctx context.Context) (any, error) {
	var out any
	e := c.request(ctx, "GET", "/healthz", nil, &out)
	return out, e
}
func (c Client) List(ctx context.Context, kind, key string, limit, offset int) (Page, error) {
	kind = WireKind(kind)
	var p Page
	if !ValidKind(kind) {
		return p, fmt.Errorf("kind must be tagtypes, flows, or programs")
	}
	if limit < 1 || limit > 1000 || offset < 0 {
		return p, fmt.Errorf("invalid pagination")
	}
	path := "/api/v1/" + kind
	if key != "" {
		if !ValidIdentifier(key) {
			return p, fmt.Errorf("invalid identifier")
		}
		path += "/" + key + "/versions"
	}
	path += fmt.Sprintf("?limit=%d&offset=%d", limit, offset)
	e := c.request(ctx, "GET", path, nil, &p)
	return p, e
}
func ParseRef(kind, text string) (Ref, error) {
	r := Ref{Kind: WireKind(kind)}
	parts := strings.Split(text, "@")
	if len(parts) > 2 {
		return r, fmt.Errorf("expected key or key@version")
	}
	r.Identifier = parts[0]
	if len(parts) == 2 {
		n, e := strconv.Atoi(parts[1])
		if e != nil || n < 1 {
			return r, fmt.Errorf("version must be a positive integer")
		}
		r.Version = n
	}
	if !ValidKind(r.Kind) || !ValidIdentifier(r.Identifier) {
		return r, fmt.Errorf("invalid %s identifier", kind)
	}
	return r, nil
}
func (c Client) Get(ctx context.Context, ref Ref) (Record, error) {
	var raw json.RawMessage
	path := "/api/v1/" + ref.Kind + "/" + ref.Identifier
	if ref.Version > 0 {
		path += fmt.Sprintf("/versions/%d", ref.Version)
	}
	if e := c.request(ctx, "GET", path, nil, &raw); e != nil {
		return Record{}, e
	}
	record, e := Decode(ref.Kind, raw)
	if e == nil && (record.Ref.Identifier != ref.Identifier || (ref.Version != 0 && record.Ref.Version != ref.Version)) {
		e = fmt.Errorf("hub returned a different entity")
	}
	return record, e
}
func (c Client) Publish(ctx context.Context, kind string, raw []byte) (Record, error) {
	kind = WireKind(kind)
	if len(raw) > 1<<20 {
		return Record{}, fmt.Errorf("manifest exceeds 1 MiB")
	}
	if c.Token == "" {
		return Record{}, fmt.Errorf("set MF_HUB_ADMIN_TOKEN to publish")
	}
	if _, e := Decode(kind, raw); e != nil {
		return Record{}, e
	}
	var out json.RawMessage
	if e := c.request(ctx, "POST", "/api/v1/"+kind, raw, &out); e != nil {
		return Record{}, e
	}
	return Decode(kind, out)
}
func (c Client) Resolve(ctx context.Context, ref Ref, osName, arch string) (Resolution, error) {
	if osName == "" {
		osName = runtime.GOOS
	}
	if arch == "" {
		arch = runtime.GOARCH
	}
	root, e := c.Get(ctx, ref)
	if e != nil {
		return Resolution{}, e
	}
	if ref.Kind != Flows {
		return Resolve(ctx, root, c.Get, osName, arch)
	}
	var out Resolution
	path := fmt.Sprintf("/api/v1/flows/%s/versions/%d/resolve?os=%s&arch=%s", root.Ref.Identifier, root.Ref.Version, url.QueryEscape(osName), url.QueryEscape(arch))
	if e = c.request(ctx, "GET", path, nil, &out); e != nil {
		return out, e
	}
	// Re-resolve exact immutable manifests and compare the server plan. Never trust
	// selected artifacts or dependency lists independently of their manifests.
	records := map[Ref]Record{}
	for _, v := range out.TagTypes {
		raw, _ := json.Marshal(v)
		r, e := Decode(TagTypes, raw)
		if e != nil {
			return out, e
		}
		if _, ok := records[r.Ref]; ok {
			return out, fmt.Errorf("duplicate resolution entity")
		}
		records[r.Ref] = r
	}
	for _, v := range out.Programs {
		raw, _ := json.Marshal(v.Program)
		r, e := Decode(Programs, raw)
		if e != nil {
			return out, e
		}
		if _, ok := records[r.Ref]; ok {
			return out, fmt.Errorf("duplicate resolution entity")
		}
		records[r.Ref] = r
	}
	for _, v := range out.Flows {
		raw, _ := json.Marshal(v.Flow)
		r, e := Decode(Flows, raw)
		if e != nil {
			return out, e
		}
		if _, ok := records[r.Ref]; ok {
			return out, fmt.Errorf("duplicate resolution entity")
		}
		records[r.Ref] = r
	}
	normalized, e := Resolve(ctx, root, func(_ context.Context, r Ref) (Record, error) {
		v, ok := records[r]
		if !ok {
			return v, fmt.Errorf("resolution missing %s", r)
		}
		return v, nil
	}, osName, arch)
	if e != nil {
		return out, e
	}
	a, _ := json.Marshal(normalized)
	b, _ := json.Marshal(out)
	if !bytes.Equal(a, b) {
		return out, fmt.Errorf("hub resolution does not match exact manifests")
	}
	return normalized, nil
}
