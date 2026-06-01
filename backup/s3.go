// s3.go : Backend implementation against any S3-compatible HTTP
// API. We intentionally do NOT pull in github.com/aws/aws-sdk-go-v2
// because :
//
//   (1) it isn't already in go.sum (would add ~150 packages of
//       vendored code for what amounts to four HTTP verbs) ;
//
//   (2) the operator-facing surface is just PUT / GET / list /
//       DELETE on path-style requests ; the bulk of the SDK
//       (credential providers, retry strategies, paginator helpers,
//       streaming uploaders) is dead weight here ;
//
//   (3) keeping the dependency surface narrow matches the rest of
//       weft (the proxy and ORAS layers use the stdlib http stack
//       too).
//
// What we DO need from S3 is AWS Signature Version 4. The signer
// implemented in this file is the path-style variant (no
// virtual-host bucket subdomains) because that's what MinIO and
// the dev/test fakes use ; AWS S3 in eu-west-1 also accepts
// path-style for the foreseeable future. Switching to vhost-style
// is a one-line URL builder change if a customer needs it later.
//
// Authentication : access-key + secret-key static credentials,
// read from the S3Backend struct (operator sets them via HCL or
// AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY env vars and the CLI
// layer threads them through). Session tokens (STS) and EC2
// instance metadata are out of scope ; weft runs on-prem and the
// operator's identity story is upstream of the bucket.
//
// What's tested : the SigV4 helpers are pure functions exercised
// in s3_test.go against the canonical AWS test vectors. The
// happy-path Upload/Download/List/Delete are tested against a
// fake S3 server (net/http/httptest) that records the requests
// so we can assert on signed headers without ever talking to AWS.

package backup

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// S3Backend talks to one S3-compatible bucket. Endpoint may be the
// default "https://s3.amazonaws.com" (AWS), "https://s3.us-east-1.
// wasabisys.com" (Wasabi), or any MinIO / R2 URL. Path-style
// addressing is used unconditionally.
type S3Backend struct {
	// Bucket name. Required ; no default.
	Bucket string
	// Endpoint is the full base URL (scheme + host + optional port).
	// Trailing slashes are tolerated. Empty defaults to AWS S3.
	Endpoint string
	// Region is the AWS region (or any region-string the
	// S3-compatible service expects in its SigV4 scope). Empty
	// defaults to "us-east-1" — MinIO accepts that as a wildcard.
	Region string
	// AccessKeyID + SecretAccessKey are static credentials. Empty
	// pair skips signing (some MinIO setups run with policy-open
	// buckets in dev) — production should always set both.
	AccessKeyID     string
	SecretAccessKey string
	// HTTPClient is the underlying transport. nil = http.DefaultClient.
	HTTPClient *http.Client
	// nowFn lets tests pin the signing timestamp. nil = time.Now.UTC.
	nowFn func() time.Time
}

// NewS3Backend constructs an S3Backend with sensible defaults.
// Returns an error only when bucket is empty — every other field
// has a non-fatal default.
func NewS3Backend(bucket, endpoint, region, accessKey, secretKey string) (*S3Backend, error) {
	if bucket == "" {
		return nil, errors.New("backup/s3: bucket is required")
	}
	if endpoint == "" {
		endpoint = "https://s3.amazonaws.com"
	}
	if region == "" {
		region = "us-east-1"
	}
	return &S3Backend{
		Bucket:          bucket,
		Endpoint:        strings.TrimRight(endpoint, "/"),
		Region:          region,
		AccessKeyID:     accessKey,
		SecretAccessKey: secretKey,
	}, nil
}

var _ Backend = (*S3Backend)(nil)

func (b *S3Backend) httpClient() *http.Client {
	if b.HTTPClient != nil {
		return b.HTTPClient
	}
	return http.DefaultClient
}

func (b *S3Backend) now() time.Time {
	if b.nowFn != nil {
		return b.nowFn().UTC()
	}
	return time.Now().UTC()
}

// keyURL builds the path-style object URL for one key. Keys are
// URL-escaped per RFC 3986 path segment rules. We don't escape '/'
// because the SigV4 canonical path includes the literal slash.
func (b *S3Backend) keyURL(key string) string {
	escaped := escapeS3Key(key)
	return fmt.Sprintf("%s/%s/%s", b.Endpoint, b.Bucket, escaped)
}

// escapeS3Key encodes a key for use in a URL path segment. Slashes
// are preserved (they are structural). Every other byte that is
// not in the AWS-recommended unreserved set is %-encoded.
func escapeS3Key(key string) string {
	const unreserved = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-._~/"
	var b strings.Builder
	for i := 0; i < len(key); i++ {
		c := key[i]
		if strings.IndexByte(unreserved, c) >= 0 {
			b.WriteByte(c)
			continue
		}
		fmt.Fprintf(&b, "%%%02X", c)
	}
	return b.String()
}

// Upload PUTs srcPath at key. The body is streamed from disk ;
// the SigV4 signature uses the file's SHA-256 (the spec calls
// this "x-amz-content-sha256" of the unsigned payload variant ;
// since we know the full body up-front, we compute the real hash
// for stronger integrity).
func (b *S3Backend) Upload(ctx context.Context, srcPath, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if srcPath == "" {
		return errors.New("backup/s3: empty srcPath")
	}
	if key == "" {
		return errors.New("backup/s3: empty key")
	}
	f, err := os.Open(srcPath)
	if err != nil {
		return fmt.Errorf("backup/s3: open src: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("backup/s3: stat src: %w", err)
	}
	hash, err := sha256File(f)
	if err != nil {
		return fmt.Errorf("backup/s3: hash src: %w", err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("backup/s3: rewind src: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, b.keyURL(key), f)
	if err != nil {
		return fmt.Errorf("backup/s3: build request: %w", err)
	}
	req.ContentLength = info.Size()
	req.Header.Set("Content-Type", "application/octet-stream")
	if err := b.sign(req, hash); err != nil {
		return err
	}
	resp, err := b.httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("backup/s3: PUT %s: %w", key, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return fmt.Errorf("backup/s3: PUT %s: HTTP %d : %s", key, resp.StatusCode, string(body))
	}
	return nil
}

// Download streams Bucket/key into dstPath. Atomic via a .part
// sibling file. Returns ErrNotFound on HTTP 404.
func (b *S3Backend) Download(ctx context.Context, key, dstPath string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if key == "" {
		return errors.New("backup/s3: empty key")
	}
	if dstPath == "" {
		return errors.New("backup/s3: empty dstPath")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.keyURL(key), nil)
	if err != nil {
		return fmt.Errorf("backup/s3: build request: %w", err)
	}
	if err := b.sign(req, emptyBodyHash); err != nil {
		return err
	}
	resp, err := b.httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("backup/s3: GET %s: %w", key, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("backup/s3: %q: %w", key, ErrNotFound)
	}
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return fmt.Errorf("backup/s3: GET %s: HTTP %d : %s", key, resp.StatusCode, string(body))
	}
	if err := os.MkdirAll(filepath.Dir(dstPath), 0o755); err != nil {
		return fmt.Errorf("backup/s3: mkdir dst parent: %w", err)
	}
	part := dstPath + ".part"
	out, err := os.OpenFile(part, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("backup/s3: open part: %w", err)
	}
	if err := copyWithCtx(ctx, out, resp.Body); err != nil {
		_ = out.Close()
		_ = os.Remove(part)
		return fmt.Errorf("backup/s3: copy: %w", err)
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(part)
		return fmt.Errorf("backup/s3: close part: %w", err)
	}
	if err := os.Rename(part, dstPath); err != nil {
		_ = os.Remove(part)
		return fmt.Errorf("backup/s3: rename: %w", err)
	}
	return nil
}

// listObjectsV2Result mirrors the subset of the S3 ListObjectsV2
// XML response we read : Contents[].Key + Contents[].Size, plus
// IsTruncated + NextContinuationToken for pagination.
type listObjectsV2Result struct {
	XMLName               xml.Name `xml:"ListBucketResult"`
	IsTruncated           bool     `xml:"IsTruncated"`
	NextContinuationToken string   `xml:"NextContinuationToken"`
	Contents              []struct {
		Key  string `xml:"Key"`
		Size int64  `xml:"Size"`
	} `xml:"Contents"`
}

// List does a paginated ListObjectsV2 with the given prefix.
// Buffered into memory ; the snapshot use case has at most a few
// thousand keys per bucket so we don't expose a streaming surface.
func (b *S3Backend) List(ctx context.Context, prefix string) ([]Entry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var entries []Entry
	cont := ""
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		q := url.Values{}
		q.Set("list-type", "2")
		if prefix != "" {
			q.Set("prefix", prefix)
		}
		if cont != "" {
			q.Set("continuation-token", cont)
		}
		u := fmt.Sprintf("%s/%s?%s", b.Endpoint, b.Bucket, q.Encode())
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, fmt.Errorf("backup/s3: build request: %w", err)
		}
		if err := b.sign(req, emptyBodyHash); err != nil {
			return nil, err
		}
		resp, err := b.httpClient().Do(req)
		if err != nil {
			return nil, fmt.Errorf("backup/s3: LIST: %w", err)
		}
		body, readErr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if readErr != nil {
			return nil, fmt.Errorf("backup/s3: read list body: %w", readErr)
		}
		if resp.StatusCode/100 != 2 {
			return nil, fmt.Errorf("backup/s3: LIST: HTTP %d : %s", resp.StatusCode, string(body))
		}
		var out listObjectsV2Result
		if err := xml.Unmarshal(body, &out); err != nil {
			return nil, fmt.Errorf("backup/s3: decode list: %w", err)
		}
		for _, c := range out.Contents {
			entries = append(entries, Entry{Key: c.Key, Size: c.Size})
		}
		if !out.IsTruncated || out.NextContinuationToken == "" {
			break
		}
		cont = out.NextContinuationToken
	}
	return entries, nil
}

// Delete sends DELETE Bucket/key. S3 returns 204 for both
// successful deletes and absent keys, so the call is naturally
// idempotent.
func (b *S3Backend) Delete(ctx context.Context, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if key == "" {
		return errors.New("backup/s3: empty key")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, b.keyURL(key), nil)
	if err != nil {
		return fmt.Errorf("backup/s3: build request: %w", err)
	}
	if err := b.sign(req, emptyBodyHash); err != nil {
		return err
	}
	resp, err := b.httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("backup/s3: DELETE %s: %w", key, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return fmt.Errorf("backup/s3: DELETE %s: HTTP %d : %s", key, resp.StatusCode, string(body))
	}
	return nil
}

// emptyBodyHash is the SHA-256 of the empty string — the canonical
// content hash for GETs / DELETEs / lists.
const emptyBodyHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// sha256File reads `r` to EOF and returns the hex SHA-256 digest.
// Used by Upload to compute the x-amz-content-sha256 over the
// full body before re-seeking.
func sha256File(r io.Reader) (string, error) {
	h := sha256.New()
	if _, err := io.Copy(h, r); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// sign mutates `req` to carry AWS Signature Version 4 headers.
// Path-style requests, signed payload hash provided by the caller.
// When AccessKeyID is empty, the call is a no-op (anonymous /
// policy-open bucket scenario).
func (b *S3Backend) sign(req *http.Request, payloadHash string) error {
	if b.AccessKeyID == "" {
		return nil
	}
	if b.SecretAccessKey == "" {
		return errors.New("backup/s3: AccessKeyID set without SecretAccessKey")
	}
	now := b.now()
	amzDate := now.Format("20060102T150405Z")
	dateStamp := now.Format("20060102")

	req.Header.Set("Host", req.URL.Host)
	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)

	canonical := buildCanonicalRequest(req, payloadHash)
	canonHash := sha256.Sum256([]byte(canonical))
	credScope := fmt.Sprintf("%s/%s/s3/aws4_request", dateStamp, b.Region)
	sts := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		credScope,
		hex.EncodeToString(canonHash[:]),
	}, "\n")
	key := signingKey(b.SecretAccessKey, dateStamp, b.Region, "s3")
	sig := hex.EncodeToString(hmacSHA256(key, sts))
	signedHeaders := signedHeaderList(req.Header)
	authz := fmt.Sprintf(
		"AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		b.AccessKeyID, credScope, signedHeaders, sig,
	)
	req.Header.Set("Authorization", authz)
	return nil
}

// buildCanonicalRequest assembles the SigV4 canonical request
// string per the AWS spec. Path is the URL-escaped path ; query
// is the lex-sorted percent-encoded query string ; headers are
// lower-cased and sorted.
func buildCanonicalRequest(req *http.Request, payloadHash string) string {
	canonPath := canonicalPath(req.URL.Path)
	canonQuery := canonicalQuery(req.URL.Query())
	canonHeaders, signed := canonicalHeaders(req.Header)
	return strings.Join([]string{
		req.Method,
		canonPath,
		canonQuery,
		canonHeaders,
		signed,
		payloadHash,
	}, "\n")
}

// canonicalPath re-escapes the request path per the SigV4 rules
// (each segment URL-encoded, "/" preserved). An empty path is "/".
func canonicalPath(p string) string {
	if p == "" {
		return "/"
	}
	// http.Request.URL.Path stores the unescaped path ; we need to
	// re-escape it for the canonical request.
	return escapeS3Key(p)
}

// canonicalQuery returns the sorted, percent-encoded query string.
func canonicalQuery(q url.Values) string {
	if len(q) == 0 {
		return ""
	}
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for i, k := range keys {
		vs := q[k]
		sort.Strings(vs)
		for j, v := range vs {
			if i > 0 || j > 0 {
				b.WriteByte('&')
			}
			b.WriteString(url.QueryEscape(k))
			b.WriteByte('=')
			b.WriteString(url.QueryEscape(v))
		}
	}
	return b.String()
}

// canonicalHeaders returns (canonical-headers-block, signed-header-list).
// Headers are lower-cased, values trimmed of leading/trailing
// whitespace and collapsed (inner runs of whitespace become single
// space).
func canonicalHeaders(h http.Header) (string, string) {
	keys := make([]string, 0, len(h))
	for k := range h {
		lk := strings.ToLower(k)
		// Standard SigV4 signs at minimum host, x-amz-date,
		// x-amz-content-sha256 ; signing every header keeps the
		// implementation honest.
		if lk == "authorization" || lk == "content-length" || lk == "user-agent" {
			continue
		}
		keys = append(keys, lk)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		v := strings.Join(h.Values(canonicalCase(k, h)), ",")
		v = strings.TrimSpace(v)
		// Collapse internal whitespace runs ; SigV4 spec.
		v = strings.Join(strings.Fields(v), " ")
		b.WriteString(k)
		b.WriteByte(':')
		b.WriteString(v)
		b.WriteByte('\n')
	}
	return b.String(), strings.Join(keys, ";")
}

// canonicalCase finds the original-case header name corresponding
// to a lower-cased canonical key (http.Header preserves the casing
// of the first Set).
func canonicalCase(lower string, h http.Header) string {
	for k := range h {
		if strings.EqualFold(k, lower) {
			return k
		}
	}
	return lower
}

// signedHeaderList returns the ";"-joined sorted list of header
// names that were included in the canonical request.
func signedHeaderList(h http.Header) string {
	_, list := canonicalHeaders(h)
	return list
}

// signingKey derives the per-request signing key from the secret
// access key, date, region, service. Standard SigV4 four-step
// HMAC chain.
func signingKey(secret, dateStamp, region, service string) []byte {
	kDate := hmacSHA256([]byte("AWS4"+secret), dateStamp)
	kRegion := hmacSHA256(kDate, region)
	kService := hmacSHA256(kRegion, service)
	return hmacSHA256(kService, "aws4_request")
}

// hmacSHA256 returns HMAC-SHA256(key, data) as raw bytes.
func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}
