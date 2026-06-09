package wefthcl

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/gocty"
)

// supportedVersion is the only config schema version currently supported.
const supportedVersion = "1"

// versionLineRe matches a top-level `version = "..."` or `version = '...'` line.
var versionLineRe = regexp.MustCompile(`(?m)^\s*version\s*=\s*["']([^"']+)["']\s*$`)

// archTokenRe matches arch.gnu and arch.oci tokens, bare or inside ${...}.
var archTokenRe = regexp.MustCompile(`\$\{arch\.(gnu|oci)\}|arch\.(gnu|oci)`)

// resolveConfig reads all *.hcl files from configDir, validates that each
// declares `version = "1"` at the top level, strips the version lines and
// concatenates all content into a single HCL blob ready for parsing.
// configDir must be a directory; passing a file path is an error.
func resolveConfig(configDir string) ([]byte, string, error) {
	if configDir == "" {
		configDir = "state/hcl"
	}
	fi, err := os.Stat(configDir)
	if err != nil {
		return nil, configDir, fmt.Errorf("config path %q not found: %w", configDir, err)
	}
	if !fi.IsDir() {
		return nil, configDir, fmt.Errorf("config path %q must be a directory containing *.hcl files", configDir)
	}

	// collect and merge all *.hcl files
	entries, err := os.ReadDir(configDir)
	if err != nil {
		return nil, configDir, fmt.Errorf("cannot read config dir %q: %w", configDir, err)
	}
	var hclFiles []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".hcl") {
			hclFiles = append(hclFiles, filepath.Join(configDir, e.Name()))
		}
	}
	if len(hclFiles) == 0 {
		return nil, configDir, fmt.Errorf("no *.hcl files found in config dir %q", configDir)
	}
	sort.Strings(hclFiles) // deterministic merge order

	var buf bytes.Buffer
	for _, path := range hclFiles {
		data, err := readAndValidateHCLFile(path)
		if err != nil {
			return nil, configDir, err
		}
		// separate files with a newline so block boundaries are preserved
		buf.Write(data)
		buf.WriteByte('\n')
	}
	// Use the directory as the nominal config path for error messages.
	return buf.Bytes(), configDir, nil
}

// readAndValidateHCLFile reads an HCL file from disk, verifies it declares
// `version = "1"` and returns the content stripped of version lines.
func readAndValidateHCLFile(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("cannot read config file %q: %w", path, err)
	}
	// Normalize typographic/curly quotes to ASCII straight quotes so that
	// copy-pasted HCL values (e.g. from browsers or word processors) parse
	// correctly instead of silently producing wrong values.
	// U+201C LEFT DOUBLE QUOTATION MARK  "  → "
	// U+201D RIGHT DOUBLE QUOTATION MARK "  → "
	// U+2018 LEFT SINGLE QUOTATION MARK  '  → '
	// U+2019 RIGHT SINGLE QUOTATION MARK '  → '
	data = bytes.ReplaceAll(data, []byte("\xe2\x80\x9c"), []byte(`"`)) // "
	data = bytes.ReplaceAll(data, []byte("\xe2\x80\x9d"), []byte(`"`)) // "
	data = bytes.ReplaceAll(data, []byte("\xe2\x80\x98"), []byte(`'`)) // '
	data = bytes.ReplaceAll(data, []byte("\xe2\x80\x99"), []byte(`'`)) // '
	m := versionLineRe.FindSubmatch(data)
	if m == nil {
		return nil, fmt.Errorf("config file %q is missing required top-level attribute: version = %q", path, supportedVersion)
	}
	ver := string(m[1])
	if ver != supportedVersion {
		return nil, fmt.Errorf("config file %q declares unsupported version %q (only %q is supported)", path, ver, supportedVersion)
	}
	// strip the version line(s) so the merged blob parses cleanly
	stripped := versionLineRe.ReplaceAll(data, []byte{})
	return stripped, nil
}

// ReadConfig returns the merged content of all *.hcl files under dir.
// It is the exported entry point for external packages that need the raw
// merged HCL bytes (e.g. for regex-based parsing).
func ReadConfig(dir string) ([]byte, error) {
	data, _, err := resolveConfig(dir)
	return data, err
}

// LoadKeypairs parses all `keypair <name> { file_path = "..." }` blocks from
// the config and returns a map[name]expandedPrivateKeyPath. The file_path value
// is expanded (~ → home directory). The path points to the private key; the
// public key is derived by appending ".pub".
func LoadKeypairs(cfg string) map[string]string {
	if cfg == "" {
		cfg = "state/hcl"
	}
	data, err := ReadConfig(cfg)
	if err != nil {
		return nil
	}
	s := stripHCLComments(string(data))
	result := make(map[string]string)
	re := regexp.MustCompile(`(?m)keypair\s+(?:"([^"]+)"|([A-Za-z_][A-Za-z0-9_-]*))\s*\{`)
	for _, loc := range re.FindAllStringSubmatchIndex(s, -1) {
		name := ""
		if loc[2] != -1 {
			name = s[loc[2]:loc[3]]
		} else if loc[4] != -1 {
			name = s[loc[4]:loc[5]]
		}
		openBrace := loc[1] - 1
		body, _, ok := extractCurlyBody(s, openBrace)
		if !ok || name == "" {
			continue
		}
		fpRe := regexp.MustCompile(`file_path\s*=\s*"([^"]+)"`)
		if m := fpRe.FindStringSubmatch(body); m != nil {
			result[name] = expandHomePath(m[1])
		}
	}
	return result
}

// expandHomePath replaces a leading ~ with the user home directory.
func expandHomePath(p string) string {
	if !strings.HasPrefix(p, "~/") && p != "~" {
		return p
	}
	hd, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	if p == "~" {
		return hd
	}
	return filepath.Join(hd, p[2:])
}

// LoadOCIFroms parses all *.hcl files in cfg (a directory) and returns
// map[name]imageRef. Supports `from = "..."` and `from = join("/", [...])`.
func LoadOCIFroms(cfg string) map[string]string {
	if cfg == "" {
		cfg = "state/hcl"
	}
	data, label, err := resolveConfig(cfg)
	if err != nil {
		return nil
	}
	s := string(data)
	vars := parseHCLVars(s)

	// try HCL syntax parse
	file, diags := hclsyntax.ParseConfig(data, label, hcl.InitialPos)
	if diags != nil && diags.HasErrors() {
		// fallback to regex (pass raw merged bytes as sentinel file path)
		return loadOCIFromsFromData(s, vars)
	}

	evalCtx := buildEvalCtxFromFile(vars, file)
	hclResult := parseOCIBlocks(file, evalCtx)
	// Merge regex-based results for image blocks whose `from` expressions
	// reference endpoint names that contain hyphens (e.g. "rocky-10").
	// HCL's expression grammar treats "-" as subtraction, so those traversals
	// fail silently in the HCL path; the regex path handles them correctly.
	for k, v := range loadOCIFromsFromData(s, vars) {
		if _, ok := hclResult[k]; !ok {
			hclResult[k] = v
		}
	}
	return hclResult
}

// LoadHTTPChecksums parses all *.hcl files in cfg and returns a map of
// imageFromURL → checksumURL for every `image` block that declares both
// a `from` attribute (resolving to an HTTPS URL) and a `checksum` attribute.
func LoadHTTPChecksums(cfg string) map[string]string {
	if cfg == "" {
		cfg = "state/hcl"
	}
	data, label, err := resolveConfig(cfg)
	if err != nil {
		return nil
	}
	s := string(data)
	vars := parseHCLVars(s)

	file, diags := hclsyntax.ParseConfig(data, label, hcl.InitialPos)
	if diags != nil && diags.HasErrors() {
		return parseHTTPChecksumsFromData(s, vars)
	}

	evalCtx := buildEvalCtxFromFile(vars, file)
	hclResult := parseChecksumBlocks(file, evalCtx)
	// Same hyphen-in-endpoint-name fallback as LoadOCIFroms.
	for k, v := range parseHTTPChecksumsFromData(s, vars) {
		if _, ok := hclResult[k]; !ok {
			hclResult[k] = v
		}
	}
	return hclResult
}

// parseChecksumBlocks extracts `image` blocks that carry both `from` and
// `checksum` attributes and returns map[fromURL]checksumURL.
func parseChecksumBlocks(file *hcl.File, evalCtx *hcl.EvalContext) map[string]string {
	out := map[string]string{}
	hb, ok := file.Body.(*hclsyntax.Body)
	if !ok {
		return out
	}
	for _, b := range hb.Blocks {
		if b.Type != "image" || len(b.Labels) < 1 {
			continue
		}
		fromAttr, hasFrom := b.Body.Attributes["from"]
		csAttr, hasCS := b.Body.Attributes["checksum"]
		if !hasFrom || !hasCS {
			continue
		}
		fromURL := parseFromExpr(fromAttr.Expr, evalCtx)
		csURL := parseFromExpr(csAttr.Expr, evalCtx)
		if fromURL != "" && csURL != "" {
			out[fromURL] = csURL
		}
	}
	return out
}

// parseHTTPChecksumsFromData is the regex fallback for LoadHTTPChecksums.
func parseHTTPChecksumsFromData(s string, vars map[string]string) map[string]string {
	out := map[string]string{}
	registries := parseRegistriesRegex(s)
	headerRe := regexp.MustCompile(`image\s+([A-Za-z0-9_-]+)\s*\{`)
	offset := 0
	for {
		loc := headerRe.FindStringSubmatchIndex(s[offset:])
		if loc == nil {
			break
		}
		openPos := offset + loc[1] - 1
		body, end, ok := extractCurlyBody(s, openPos)
		if !ok {
			break
		}
		offset = end

		fromURL := ""
		if fr := regexp.MustCompile(`from\s*=\s*"([^"]+)"`); fr.MatchString(body) {
			fromURL = expandArchTemplate(fr.FindStringSubmatch(body)[1])
		} else if v, ok2 := tryParseJoinArray(body, vars, registries); ok2 {
			fromURL = v
		}
		csURL := ""
		if cr := regexp.MustCompile(`checksum\s*=\s*"([^"]+)"`); cr.MatchString(body) {
			csURL = expandArchTemplate(cr.FindStringSubmatch(body)[1])
		} else {
			// attempt join() parse on the checksum line only
			if ci := strings.Index(body, "checksum"); ci >= 0 {
				csPart := body[ci:]
				if v, ok2 := tryParseJoinArray(csPart, vars, registries); ok2 {
					csURL = v
				}
			}
		}
		if fromURL != "" && csURL != "" {
			out[fromURL] = csURL
		}
	}
	return out
}

// buildEvalCtxFromFile builds an HCL eval context including `var`, `endpoint`,
// buildEvalCtxFromFile builds an HCL eval context including `var`, `arch`, and `endpoint`.
func buildEvalCtxFromFile(vars map[string]string, file *hcl.File) *hcl.EvalContext {
	evalVars := map[string]cty.Value{}
	for k, v := range vars {
		evalVars[k] = cty.StringVal(v)
	}
	gnuArch, _ := goArchFormat("gnu")
	ociArch, _ := goArchFormat("oci")
	evalCtx := &hcl.EvalContext{
		Variables: map[string]cty.Value{
			"var": cty.ObjectVal(evalVars),
			"arch": cty.ObjectVal(map[string]cty.Value{
				"gnu": cty.StringVal(gnuArch),
				"oci": cty.StringVal(ociArch),
			}),
		},
	}
	endpointObj := map[string]cty.Value{}
	if hb, ok := file.Body.(*hclsyntax.Body); ok {
		for _, b := range hb.Blocks {
			if b.Type != "endpoint" || len(b.Labels) < 1 {
				continue
			}
			lname := b.Labels[0]
			if attr, ok := b.Body.Attributes["url"]; ok {
				if val, diags := attr.Expr.Value(evalCtx); diags == nil || !diags.HasErrors() {
					if val.Type() == cty.String {
						obj := cty.ObjectVal(map[string]cty.Value{"url": cty.StringVal(val.AsString())})
						endpointObj[lname] = obj
					}
				}
			}
		}
	}
	if len(endpointObj) > 0 {
		evalCtx.Variables["endpoint"] = cty.ObjectVal(endpointObj)
	}
	// parse keypair blocks so that `keypair.<name>.file_path` resolves in expressions
	keypairObj := map[string]cty.Value{}
	if hb, ok := file.Body.(*hclsyntax.Body); ok {
		for _, b := range hb.Blocks {
			if b.Type != "keypair" || len(b.Labels) < 1 {
				continue
			}
			lname := b.Labels[0]
			if attr, ok2 := b.Body.Attributes["file_path"]; ok2 {
				if val, diags := attr.Expr.Value(evalCtx); diags == nil || !diags.HasErrors() {
					if val.Type() == cty.String {
						obj := cty.ObjectVal(map[string]cty.Value{
							"file_path": cty.StringVal(expandHomePath(val.AsString())),
						})
						keypairObj[lname] = obj
					}
				}
			}
		}
	}
	if len(keypairObj) > 0 {
		evalCtx.Variables["keypair"] = cty.ObjectVal(keypairObj)
	}
	return evalCtx
}

// parseOCIBlocks extracts `image` blocks from the parsed HCL file and
// resolves their `from` expressions using the provided eval context.
func parseOCIBlocks(file *hcl.File, evalCtx *hcl.EvalContext) map[string]string {
	out := map[string]string{}
	if hb, ok := file.Body.(*hclsyntax.Body); ok {
		for _, b := range hb.Blocks {
			if b.Type != "image" || len(b.Labels) < 1 {
				continue
			}
			name := b.Labels[0]
			if attr, ok := b.Body.Attributes["from"]; ok {
				if v := parseFromExpr(attr.Expr, evalCtx); v != "" {
					out[name] = v
				}
			}
		}
	}
	return out
}

// parseFromExpr handles `from` expressions, supporting `join()` and simple
// string expressions.
func parseFromExpr(expr hclsyntax.Expression, evalCtx *hcl.EvalContext) string {
	if fn, ok := expr.(*hclsyntax.FunctionCallExpr); ok && fn.Name == "join" && len(fn.Args) >= 2 {
		sepVal, sd := fn.Args[0].Value(evalCtx)
		if sd.HasErrors() {
			return ""
		}
		if sepVal.Type() != cty.String {
			return ""
		}
		sep := sepVal.AsString()
		if tuple, ok := fn.Args[1].(*hclsyntax.TupleConsExpr); ok {
			parts := []string{}
			for _, item := range tuple.Exprs {
				if v, d := item.Value(evalCtx); !d.HasErrors() {
					parts = append(parts, strings.TrimSuffix(v.AsString(), sep))
				}
			}
			if len(parts) > 0 {
				return strings.Join(parts, sep)
			}
			return ""
		}
		if lv, ld := fn.Args[1].Value(evalCtx); !ld.HasErrors() {
			var parts []string
			if err := gocty.FromCtyValue(lv, &parts); err == nil && len(parts) > 0 {
				for i := range parts {
					parts[i] = strings.TrimSuffix(parts[i], sep)
				}
				return strings.Join(parts, sep)
			}
			var any []interface{}
			if err := gocty.FromCtyValue(lv, &any); err == nil && len(any) > 0 {
				segs := []string{}
				for _, it := range any {
					switch v := it.(type) {
					case string:
						segs = append(segs, strings.TrimSuffix(v, sep))
					default:
						segs = append(segs, fmt.Sprintf("%v", v))
					}
				}
				if len(segs) > 0 {
					return strings.Join(segs, sep)
				}
			}
			return ""
		}
	}
	if val, d := expr.Value(evalCtx); !d.HasErrors() {
		if val.Type() == cty.String {
			return val.AsString()
		}
	}
	return ""
}

// parseRegistriesRegex extracts url entries from `endpoint` blocks
// via regex fallback.
func parseRegistriesRegex(s string) map[string]string {
	registries := map[string]string{}
	headerRe := regexp.MustCompile(`endpoint\s+([A-Za-z0-9_-]+)\s*\{`)
	offset := 0
	for {
		loc := headerRe.FindStringSubmatchIndex(s[offset:])
		if loc == nil {
			break
		}
		name := s[offset+loc[2] : offset+loc[3]]
		openPos := offset + loc[1] - 1
		body, end, ok := extractCurlyBody(s, openPos)
		if !ok {
			break
		}
		if ur := regexp.MustCompile(`url\s*=\s*"([^"]+)"`); ur.MatchString(body) {
			registries[name] = ur.FindStringSubmatch(body)[1]
		}
		offset = end
	}
	return registries
}

// parseOCIFallbackRegex parses `image` blocks using regex fallback and
// supports join([...]) arrays, var.*, endpoint.* tokens.
func parseOCIFallbackRegex(s string, vars map[string]string, registries map[string]string) map[string]string {
	out := map[string]string{}
	headerRe := regexp.MustCompile(`image\s+([A-Za-z0-9_-]+)\s*\{`)
	offset := 0
	for {
		loc := headerRe.FindStringSubmatchIndex(s[offset:])
		if loc == nil {
			break
		}
		name := s[offset+loc[2] : offset+loc[3]]
		openPos := offset + loc[1] - 1
		body, end, ok := extractCurlyBody(s, openPos)
		if !ok {
			break
		}
		offset = end
		if fr := regexp.MustCompile(`from\s*=\s*"([^"]+)"`); fr.MatchString(body) {
			out[name] = expandArchTemplate(fr.FindStringSubmatch(body)[1])
			continue
		}
		if v, ok := tryParseJoinArray(body, vars, registries); ok {
			out[name] = v
			continue
		}
		if br := regexp.MustCompile(`from\s*=\s*([^\n\r]+)`); br.MatchString(body) {
			token := strings.TrimSpace(br.FindStringSubmatch(body)[1])
			token = strings.Trim(token, `"' `)
			out[name] = token
		}
	}
	return out
}

// tryParseJoinArray attempts to locate a join([...]) pattern in the block body
// and returns the joined value (with var and registry token resolution).
func tryParseJoinArray(body string, vars map[string]string, registries map[string]string) (string, bool) {
	idx := strings.Index(body, "join")
	if idx == -1 {
		return "", false
	}
	if lb := strings.Index(body[idx:], "["); lb == -1 {
		return "", false
	} else {
		lbIndex := idx + lb
		end := findMatchingBracket(body, lbIndex)
		if end == -1 || end <= lbIndex {
			return "", false
		}
		inner := body[lbIndex+1 : end]
		elems := splitJoinElements(inner)
		var segs []string
		for _, e := range elems {
			if s, ok := resolveJoinElement(e, vars, registries); ok {
				segs = append(segs, s)
			}
		}
		if len(segs) > 0 {
			for i := range segs {
				segs[i] = strings.TrimSuffix(segs[i], "/")
			}
			return strings.Join(segs, "/"), true
		}
		return "", false
	}
}

// findMatchingBracket finds the index of the matching ']' for the bracket
// starting at pos (which should point at '['). Returns -1 if none found.
func findMatchingBracket(s string, pos int) int {
	depth := 0
	for i := pos; i < len(s); i++ {
		ch := s[i]
		if ch == '[' {
			depth++
		} else if ch == ']' {
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// splitJoinElements splits a comma-separated list while respecting quotes.
func splitJoinElements(inner string) []string {
	var elems []string
	cur := strings.Builder{}
	var inQ rune
	for i := 0; i < len(inner); i++ {
		r := rune(inner[i])
		if inQ == 0 && (r == '"' || r == '\'') {
			inQ = r
			cur.WriteRune(r)
			continue
		}
		if inQ != 0 && r == inQ {
			cur.WriteRune(r)
			inQ = 0
			continue
		}
		if inQ == 0 && r == ',' {
			elems = append(elems, strings.TrimSpace(cur.String()))
			cur.Reset()
			continue
		}
		cur.WriteRune(r)
	}
	if cur.Len() > 0 {
		elems = append(elems, strings.TrimSpace(cur.String()))
	}
	return elems
}

// resolveJoinElement resolves a single join element token into a string value.
func resolveJoinElement(e string, vars map[string]string, registries map[string]string) (string, bool) {
	e = strings.TrimSpace(e)
	if strings.HasPrefix(e, "var.") {
		vn := strings.TrimPrefix(e, "var.")
		if v, ok := vars[vn]; ok {
			return v, true
		}
		return "", false
	}
	// `endpoint.NAME.url` is resolved via the registries map.
	if strings.HasPrefix(e, "endpoint.") {
		parts := strings.Split(e, ".")
		if len(parts) >= 3 && parts[2] == "url" {
			if v, ok := registries[parts[1]]; ok {
				return v, true
			}
		}
	}
	if len(e) >= 2 && ((e[0] == '"' && e[len(e)-1] == '"') || (e[0] == '\'' && e[len(e)-1] == '\'')) {
		inner := e[1 : len(e)-1]
		inner = expandArchTemplate(inner)
		return inner, true
	}
	// bare arch.gnu / arch.oci token
	if strings.HasPrefix(e, "arch.") {
		format := strings.TrimPrefix(e, "arch.")
		if v, err := goArchFormat(format); err == nil {
			return v, true
		}
		return "", false
	}
	return strings.Trim(e, `"' `), true
}

// goArchFormat returns the architecture string for the given naming convention.
// format "gnu" → aarch64/x86_64 (GNU/uname style, used by Rocky, Fedora, Alpine).
// format "oci" → arm64/amd64  (OCI/Docker/Debian style).
func goArchFormat(format string) (string, error) {
	switch format {
	case "gnu":
		switch runtime.GOARCH {
		case "arm64":
			return "aarch64", nil
		case "amd64":
			return "x86_64", nil
		default:
			return runtime.GOARCH, nil
		}
	case "oci":
		switch runtime.GOARCH {
		case "arm64":
			return "arm64", nil
		case "amd64":
			return "amd64", nil
		default:
			return runtime.GOARCH, nil
		}
	default:
		return "", fmt.Errorf("arch(): unknown format %q, valid values are \"gnu\" and \"oci\"", format)
	}
}

// expandArchTemplate replaces arch.gnu and arch.oci tokens (bare or wrapped
// in ${...}) in a string with the corresponding architecture value.
func expandArchTemplate(s string) string {
	return archTokenRe.ReplaceAllStringFunc(s, func(match string) string {
		subs := archTokenRe.FindStringSubmatch(match)
		format := subs[1] // group 1: ${arch.X}
		if format == "" {
			format = subs[2] // group 2: bare arch.X
		}
		if v, err := goArchFormat(format); err == nil {
			return v
		}
		return match
	})
}

// parseHCLVars extracts simple var assignments of the form: var name = "value".
func parseHCLVars(s string) map[string]string {
	vars := map[string]string{}
	varRe := regexp.MustCompile(`(?m)var\s+([A-Za-z0-9_]+)\s*=\s*"([^\"]+)"`)
	for _, m := range varRe.FindAllStringSubmatch(s, -1) {
		if len(m) >= 3 {
			vars[m[1]] = m[2]
		}
	}
	return vars
}

// LogConfig describes logging-related settings parsed from weft.hcl
type LogConfig struct {
	File       string
	Level      string
	MaxMB      int
	TimeoutSec int
}

// LoadLogConfig parses an optional `log` block from the config (directory or
// single file) and returns the discovered settings.
func LoadLogConfig(cfg string) LogConfig {
	if cfg == "" {
		cfg = "state/hcl"
	}
	data, label, err := resolveConfig(cfg)
	if err != nil {
		return LogConfig{}
	}
	file, diags := hclsyntax.ParseConfig(data, label, hcl.InitialPos)
	if diags != nil && diags.HasErrors() {
		return LogConfig{}
	}
	out := LogConfig{}
	evalCtx := &hcl.EvalContext{Variables: map[string]cty.Value{}}
	if hb, ok := file.Body.(*hclsyntax.Body); ok {
		for _, b := range hb.Blocks {
			if b.Type != "log" {
				continue
			}
			// use helpers for common attribute reads
			if v := readStringAttr(b.Body, evalCtx, "file", "path", "file_path", "file-path"); v != "" {
				out.File = v
			}
			if v := readStringAttr(b.Body, evalCtx, "level"); v != "" {
				out.Level = v
			}
			if iv, ok := readIntAttr(b.Body, evalCtx, "max_mb"); ok {
				out.MaxMB = iv
			}
			if iv, ok := readIntAttr(b.Body, evalCtx, "timeout_seconds"); ok {
				out.TimeoutSec = iv
			}
			break
		}
	}
	return out
}

// TimeoutConfig holds timeout-related settings from the weft.hcl
type TimeoutConfig struct {
	// PullPostCompletion controls how long to wait after a pull signals completion
	// before declaring the process finished (seconds).
	PullPostCompletion int
	// WaitSSH is the overall timeout (in seconds) for waiting for SSH reachability.
	WaitSSH int
	// Up is the overall timeout (in seconds) for the `up` operation (optional).
	Up int
}

// LoadTimeoutConfig parses a `mock` block and extracts a nested `timeout` block
// if present. Example:
//
//	weft "ID" {
//	  timeout {
//	    pull_post_completion = 30
//	    wait_ssh = 120
//	  }
//	}
func LoadTimeoutConfig(cfg string) TimeoutConfig {
	if cfg == "" {
		cfg = "state/hcl"
	}
	data, label, err := resolveConfig(cfg)
	if err != nil {
		return TimeoutConfig{}
	}
	file, diags := hclsyntax.ParseConfig(data, label, hcl.InitialPos)
	if diags == nil || !diags.HasErrors() {
		evalCtx := &hcl.EvalContext{Variables: map[string]cty.Value{}}
		if hb, ok := file.Body.(*hclsyntax.Body); ok {
			for _, b := range hb.Blocks {
				if b.Type != "weft" {
					continue
				}
				for _, nb := range b.Body.Blocks {
					if nb.Type != "timeout" {
						continue
					}
					return parseTimeoutBlock(nb.Body, evalCtx)
				}
			}
		}
	}
	// Fallback: regex-based extraction when HCL2 parse fails (e.g. config
	// files with block types containing hyphens that confuse the parser).
	return loadTimeoutConfigRegex(string(data))
}

// loadTimeoutConfigRegex extracts timeout settings from raw HCL content using
// regular expressions. Used as a fallback when hclsyntax.ParseConfig fails.
func loadTimeoutConfigRegex(s string) TimeoutConfig {
	s = stripHCLComments(s)
	// Locate the weft block body.
	weftRe := regexp.MustCompile(`(?m)\bweft\s+(?:"[^"]*"|[A-Za-z_][A-Za-z0-9_-]*)\s*\{`)
	loc := weftRe.FindStringIndex(s)
	if loc == nil {
		return TimeoutConfig{}
	}
	weftBody, _, ok := extractCurlyBody(s, loc[1]-1)
	if !ok {
		return TimeoutConfig{}
	}
	// Locate the timeout block inside the weft body.
	timeoutRe := regexp.MustCompile(`(?m)\btimeout\s*\{`)
	tloc := timeoutRe.FindStringIndex(weftBody)
	if tloc == nil {
		return TimeoutConfig{}
	}
	timeoutBody, _, ok := extractCurlyBody(weftBody, tloc[1]-1)
	if !ok {
		return TimeoutConfig{}
	}

	parseDur := func(keys ...string) int {
		for _, k := range keys {
			re := regexp.MustCompile(`(?m)\b` + regexp.QuoteMeta(k) + `\s*=\s*"([^"]+)"`)
			if m := re.FindStringSubmatch(timeoutBody); m != nil {
				if d, err := time.ParseDuration(m[1]); err == nil {
					return int(d.Seconds())
				}
				// plain integer in quotes
				var n int
				if _, err := fmt.Sscanf(m[1], "%d", &n); err == nil {
					return n
				}
			}
			// unquoted integer
			re2 := regexp.MustCompile(`(?m)\b` + regexp.QuoteMeta(k) + `\s*=\s*([0-9]+)\b`)
			if m := re2.FindStringSubmatch(timeoutBody); m != nil {
				var n int
				if _, err := fmt.Sscanf(m[1], "%d", &n); err == nil {
					return n
				}
			}
		}
		return 0
	}

	out := TimeoutConfig{}
	out.PullPostCompletion = parseDur("pull_post_completion", "post_completion_seconds", "pull")
	out.WaitSSH = parseDur("wait_ssh", "wait_seconds", "wait")
	out.Up = parseDur("up")
	return out
}

// parseTimeoutBlock reads known timeout attributes from the provided body.
func parseTimeoutBlock(b *hclsyntax.Body, evalCtx *hcl.EvalContext) TimeoutConfig {
	out := TimeoutConfig{}

	// durationSecs reads an attribute that may be either a plain integer
	// (seconds) or a Go duration string (e.g. "300s", "5m").
	durationSecs := func(keys ...string) (int, bool) {
		// try string first (e.g. "300s")
		if sv := readStringAttr(b, evalCtx, keys...); sv != "" {
			if d, err := time.ParseDuration(sv); err == nil {
				return int(d.Seconds()), true
			}
		}
		return readIntAttr(b, evalCtx, keys...)
	}

	if iv, ok := durationSecs("pull_post_completion", "post_completion_seconds", "pull"); ok {
		out.PullPostCompletion = iv
	}
	if iv, ok := durationSecs("wait_ssh", "wait_seconds", "wait"); ok {
		out.WaitSSH = iv
	}
	if iv, ok := durationSecs("up"); ok {
		out.Up = iv
	}
	return out
}

// WeftBlock describes a top-level `weft` block values we care about.
type WeftBlock struct {
	AuthorizedKeysPath string
	Parallelism        int
	Adapter            string
	ID                 string
	CachePath          string
	VMsPath            string
	// SSHUser is the default SSH username from `weft { ssh { user = "..." } }`.
	SSHUser string
	// SSHKeyPath is the default SSH private key path from `weft { ssh { keypair = ... } }`.
	SSHKeyPath string
}

// LoadWeftBlock parses the top-level `weft` block from the config (directory
// or single file) and returns discovered settings.
func LoadWeftBlock(cfg string) WeftBlock {
	if cfg == "" {
		cfg = "state/hcl"
	}
	data, label, err := resolveConfig(cfg)
	if err != nil {
		return WeftBlock{}
	}
	file, diags := hclsyntax.ParseConfig(data, label, hcl.InitialPos)
	if diags != nil && diags.HasErrors() {
		// fallback to regex parse
		return parseWeftBlockRegex(string(data))
	}
	evalCtx := &hcl.EvalContext{
		Variables: map[string]cty.Value{
			"adapter": cty.ObjectVal(map[string]cty.Value{
				"TART":        cty.StringVal("adapter.TART"),
				"VZ":          cty.StringVal("adapter.VZ"),
				"FIRECRACKER": cty.StringVal("adapter.FIRECRACKER"),
			}),
		},
	}
	if hb, ok := file.Body.(*hclsyntax.Body); ok {
		for _, b := range hb.Blocks {
			if b.Type != "weft" {
				continue
			}
			return parseWeftBlockHCL(b.Body, evalCtx)
		}
	}
	return WeftBlock{}
}

// parseWeftBlockRegex extracts weft block information using a regex fallback.
func parseWeftBlockRegex(s string) WeftBlock {
	s = stripHCLComments(s)
	// Use a header-only regex to find the weft block label and opening brace,
	// then use extractCurlyBody to correctly handle nested sub-blocks.
	rb := regexp.MustCompile(`(?m)weft\s+(?:"([^"]+)"|([A-Za-z_][A-Za-z0-9_-]*))\s*\{`)
	loc := rb.FindStringSubmatchIndex(s)
	if loc != nil {
		id := ""
		if loc[2] != -1 {
			id = s[loc[2]:loc[3]]
		} else if loc[4] != -1 {
			id = s[loc[4]:loc[5]]
		}
		openPos := loc[1] - 1 // position of '{'
		mb, _, ok := extractCurlyBody(s, openPos)
		if !ok {
			return WeftBlock{}
		}
		res := WeftBlock{ID: id}
		if ark := regexp.MustCompile(`authorized[_-]?keys[_-]?path\s*=\s*"([^"]+)"`); ark.MatchString(mb) {
			res.AuthorizedKeysPath = ark.FindStringSubmatch(mb)[1]
		}
		if pr := regexp.MustCompile(`parallelism\s*=\s*([0-9]+)`); pr.MatchString(mb) {
			if v, err := strconv.Atoi(pr.FindStringSubmatch(mb)[1]); err == nil {
				res.Parallelism = v
			}
		}
		if ar := regexp.MustCompile(`adapter\s*=\s*([A-Za-z0-9_.-]+)`); ar.MatchString(mb) {
			res.Adapter = ar.FindStringSubmatch(mb)[1]
		}
		if cacheLoc := regexp.MustCompile(`(?m)cache\s*\{`).FindStringIndex(mb); cacheLoc != nil {
			if cb, _, ok := extractCurlyBody(mb, cacheLoc[1]-1); ok {
				if cp := regexp.MustCompile(`path\s*=\s*"([^\"]+)"`); cp.MatchString(cb) {
					res.CachePath = cp.FindStringSubmatch(cb)[1]
				}
			}
		}
		if vmsLoc := regexp.MustCompile(`(?m)vms\s*\{`).FindStringIndex(mb); vmsLoc != nil {
			if vb, _, ok := extractCurlyBody(mb, vmsLoc[1]-1); ok {
				if vp := regexp.MustCompile(`path\s*=\s*"([^\"]+)"`); vp.MatchString(vb) {
					res.VMsPath = vp.FindStringSubmatch(vb)[1]
				}
			}
		}
		if sshLoc := regexp.MustCompile(`(?m)ssh\s*\{`).FindStringIndex(mb); sshLoc != nil {
			if sb, _, ok := extractCurlyBody(mb, sshLoc[1]-1); ok {
				if ur := regexp.MustCompile(`user\s*=\s*"([^"]+)"`); ur.MatchString(sb) {
					res.SSHUser = ur.FindStringSubmatch(sb)[1]
				}
				if kp := regexp.MustCompile(`keypair\s*=\s*(?:"([^"]+)"|keypair\.([A-Za-z_][A-Za-z0-9_-]*))`); kp.MatchString(sb) {
					m2 := kp.FindStringSubmatch(sb)
					if m2[1] != "" {
						res.SSHKeyPath = expandHomePath(m2[1])
					} else if m2[2] != "" {
						// resolve keypair.<name> reference via the already-merged config string
						kpRe := regexp.MustCompile(`(?m)keypair\s+` + regexp.QuoteMeta(m2[2]) + `\s*\{`)
						if kpLoc := kpRe.FindStringIndex(s); kpLoc != nil {
							if kb, _, ok2 := extractCurlyBody(s, kpLoc[1]-1); ok2 {
								if fp := regexp.MustCompile(`file_path\s*=\s*"([^"]+)"`); fp.MatchString(kb) {
									res.SSHKeyPath = expandHomePath(fp.FindStringSubmatch(kb)[1])
								}
							}
						}
					}
				}
			}
		}
		return res
	}
	return WeftBlock{}
}

// parseWeftBlockHCL extracts weft block values from a parsed HCL body.
func parseWeftBlockHCL(b *hclsyntax.Body, evalCtx *hcl.EvalContext) WeftBlock {
	out := WeftBlock{}
	// id is provided as label on the block; if present it will be set by caller
	if v := readStringAttr(b, evalCtx, "authorized_keys_path"); v != "" {
		out.AuthorizedKeysPath = v
	}
	if out.AuthorizedKeysPath == "" {
		if v := readStringAttr(b, evalCtx, "authorized-keys-path"); v != "" {
			out.AuthorizedKeysPath = v
		}
	}
	if iv, ok := readIntAttr(b, evalCtx, "parallelism"); ok {
		out.Parallelism = iv
	}
	if v := readStringAttr(b, evalCtx, "adapter"); v != "" {
		out.Adapter = v
	}
	for _, nb := range b.Blocks {
		if nb.Type == "cache" {
			if v := readStringAttr(nb.Body, evalCtx, "path"); v != "" {
				out.CachePath = v
			}
		}
		if nb.Type == "vms" {
			if v := readStringAttr(nb.Body, evalCtx, "path"); v != "" {
				out.VMsPath = v
			}
		}
		if nb.Type == "ssh" {
			if v := readStringAttr(nb.Body, evalCtx, "user"); v != "" {
				out.SSHUser = v
			}
			// keypair attribute: keypair.<name> reference resolved via evalCtx
			if attr, ok := nb.Body.Attributes["keypair"]; ok {
				if val, diags := attr.Expr.Value(evalCtx); diags == nil || !diags.HasErrors() {
					if val.Type() == cty.String {
						v := val.AsString()
						// value may be "keypair.<name>" token or an expanded path
						if strings.HasPrefix(v, "keypair.") {
							// traverse: keypair.<name>.file_path
							parts := strings.SplitN(v, ".", 3)
							if len(parts) >= 2 {
								if kpObj, ok2 := evalCtx.Variables["keypair"]; ok2 {
									kpMap := kpObj.AsValueMap()
									if kpEntry, ok3 := kpMap[parts[1]]; ok3 {
										if fp, ok4 := kpEntry.AsValueMap()["file_path"]; ok4 {
											out.SSHKeyPath = fp.AsString()
										}
									}
								}
							}
						} else {
							out.SSHKeyPath = expandHomePath(v)
						}
					}
				}
			}
		}
	}
	return out
}

// loadOCIFromsFromData uses the regex-based parsers to resolve oci.from values
// from an already-loaded and merged HCL content string.
func loadOCIFromsFromData(s string, vars map[string]string) map[string]string {
	registries := parseRegistriesRegex(s)
	return parseOCIFallbackRegex(s, vars, registries)
}

// readStringAttr returns the first matching string attribute value from the body.
func readStringAttr(b *hclsyntax.Body, evalCtx *hcl.EvalContext, keys ...string) string {
	for _, k := range keys {
		if attr, ok := b.Attributes[k]; ok {
			if v, d := attr.Expr.Value(evalCtx); d == nil || !d.HasErrors() {
				if v.Type() == cty.String {
					return v.AsString()
				}
			}
		}
	}
	return ""
}

// readIntAttr returns the first matching integer attribute value from the body.
func readIntAttr(b *hclsyntax.Body, evalCtx *hcl.EvalContext, keys ...string) (int, bool) {
	for _, k := range keys {
		if attr, ok := b.Attributes[k]; ok {
			if v, d := attr.Expr.Value(evalCtx); d == nil || !d.HasErrors() {
				var iv int
				if err := gocty.FromCtyValue(v, &iv); err == nil {
					return iv, true
				}
				var fv float64
				if err := gocty.FromCtyValue(v, &fv); err == nil {
					return int(fv), true
				}
			}
		}
	}
	return 0, false
}
