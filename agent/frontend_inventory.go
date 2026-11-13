package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	actool "github.com/Autumn-27/norma/tool"
	"gondar/intercept"
)

const (
	maxFrontendFiles      = 32
	maxFrontendFileBytes  = 2 << 20
	maxFrontendTotalBytes = 8 << 20
	maxFrontendCandidates = 256
	maxFrontendOutput     = 64 << 10
)

type frontendMaterial struct {
	Path      string `json:"path"`
	SourceURL string `json:"source_url"`
}

type frontendEvidence struct {
	Path      string `json:"path"`
	SourceURL string `json:"source_url"`
	SHA256    string `json:"sha256"`
	Line      int    `json:"line"`
}

type frontendCandidate struct {
	Kind        string             `json:"kind"`
	Reference   string             `json:"reference"`
	URL         string             `json:"url,omitempty"`
	SameOrigin  bool               `json:"same_origin"`
	Template    bool               `json:"template,omitempty"`
	ValueSHA256 string             `json:"value_sha256,omitempty"`
	Occurrences int                `json:"occurrences"`
	Evidence    []frontendEvidence `json:"evidence"`
}

type frontendInventory struct {
	Files      int                 `json:"files"`
	BytesRead  int                 `json:"bytes_read"`
	Candidates []frontendCandidate `json:"candidates"`
	Issues     []string            `json:"issues,omitempty"`
	Truncated  bool                `json:"truncated"`
	Note       string              `json:"note"`
}

// The literal/path classification follows api-recon's existing static harvest
// contract. This reads saved material only; it is not a browser, JS evaluator,
// source-code audit, crawler, or a source of additional target authorization.
var (
	frontendQuoted = regexp.MustCompile("[\"'`]([^\"'`\\r\\n]+)[\"'`]")
	frontendAPI    = regexp.MustCompile(`(?i)^/(api|rest|service|services|gateway|graphql|v[0-9]+|web|admin|backend|open)(/|\b)`)
	frontendAsset  = regexp.MustCompile(`(?i)\.(js|mjs|css|map)(\?|#|$)`)
	frontendMap    = regexp.MustCompile(`sourceMappingURL\s*=\s*([^\s*]+)`)
	frontendConfig = regexp.MustCompile("(?i)(base[_-]?url|api[_-]?(?:base|url|host|key|token)|client[_-]?secret)\\s*[:=]\\s*[\"'`]([^\"'`\\r\\n]+)[\"'`]")
)

func (t *ToolSet) frontendInventoryTool() actool.CoreTool {
	return readTool("frontend_inventory",
		"从当前运行目录已保存的 HTML/JS/sourcemap 提取接口、路由、chunk、配置线索及出处。只读本地材料，不联网、不执行脚本、不自动登记漏洞。外域只作线索，不扩大测试授权；动态拼接/混淆可能漏掉，结果须由已有验证流程核验。最多32文件、单文件2MiB、累计8MiB、256条线索，输出不超过64KiB。",
		obj(map[string]any{"materials": map[string]any{"type": "array", "maxItems": maxFrontendFiles, "items": obj(map[string]any{
			"path":       str("已保存文件相对当前运行目录的路径；也接受目录内绝对路径"),
			"source_url": str("该文件原始HTTP(S) URL，只作相对路径解析与出处，不发请求"),
		}, "path", "source_url")}}, "materials"),
		func(ctx context.Context, input json.RawMessage) (actool.Result, error) {
			var args struct {
				Materials []frontendMaterial `json:"materials"`
			}
			if err := json.Unmarshal(input, &args); err != nil {
				return actool.Errorf("资料参数格式无效"), nil
			}
			dir := t.runDir
			if dir == "" {
				dir = intercept.ReviewWorkingDirectory(ctx)
			}
			result, err := inspectFrontendMaterials(ctx, dir, args.Materials)
			if err != nil {
				return actool.Errorf("离线资料提取失败: " + err.Error()), nil
			}
			for {
				encoded, err := json.Marshal(result)
				if err != nil {
					return actool.Errorf("编码提取结果失败"), nil
				}
				if len(encoded) <= maxFrontendOutput {
					return actool.Text(string(encoded)), nil
				}
				result.Truncated = true
				if len(result.Candidates) > 0 {
					result.Candidates = result.Candidates[:len(result.Candidates)-1]
				} else {
					result.Issues = result.Issues[:len(result.Issues)-1]
				}
			}
		})
}

func inspectFrontendMaterials(ctx context.Context, dir string, materials []frontendMaterial) (frontendInventory, error) {
	out := frontendInventory{Candidates: make([]frontendCandidate, 0), Note: "全部为未验证线索；不代表漏洞、执行许可或新的授权目标。未抓取任何URL，未执行材料中的代码。"}
	if dir == "" || len(materials) == 0 || len(materials) > maxFrontendFiles {
		return out, errors.New("需要已绑定运行目录及1至32份材料")
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return out, err
	}
	defer root.Close()
	seenFiles, seenCandidates := map[string]bool{}, map[string]int{}
	for _, material := range materials {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		base, err := url.Parse(material.SourceURL)
		if err != nil || base.Hostname() == "" || base.User != nil || (base.Scheme != "http" && base.Scheme != "https") || len(material.SourceURL) > 2048 {
			out.Issues = append(out.Issues, "来源URL无效（仅接受无凭据HTTP(S)地址）")
			continue
		}
		rel := material.Path
		if filepath.IsAbs(rel) {
			rel, err = filepath.Rel(dir, rel)
		}
		if err != nil || !filepath.IsLocal(rel) || len(rel) > 1024 {
			out.Issues = append(out.Issues, "材料路径不在运行目录内")
			continue
		}
		rel = filepath.Clean(rel)
		fileKey := rel + "\x00" + base.String()
		if seenFiles[fileKey] {
			continue
		}
		seenFiles[fileKey] = true
		limit := min(maxFrontendFileBytes, maxFrontendTotalBytes-out.BytesRead)
		if limit <= 0 {
			out.Truncated = true
			break
		}
		info, err := root.Stat(rel)
		if err != nil || !info.Mode().IsRegular() {
			out.Issues = append(out.Issues, filepath.ToSlash(rel)+": 不是可读普通文件")
			continue
		}
		if info.Size() > int64(limit) {
			out.Issues = append(out.Issues, filepath.ToSlash(rel)+": 超出内容预算，未解析")
			out.Truncated = true
			continue
		}
		file, err := root.Open(rel)
		if err != nil {
			out.Issues = append(out.Issues, filepath.ToSlash(rel)+": 无法打开")
			continue
		}
		data, readErr := io.ReadAll(io.LimitReader(file, int64(limit)+1))
		closeErr := file.Close()
		out.BytesRead += len(data)
		if readErr != nil || closeErr != nil || len(data) > limit {
			out.Issues = append(out.Issues, filepath.ToSlash(rel)+": 读取失败或内容增长超限，未解析")
			out.Truncated = true
			continue
		}
		out.Files++
		hash := sha256.Sum256(data)
		evidence := frontendEvidence{Path: filepath.ToSlash(rel), SourceURL: base.String(), SHA256: hex.EncodeToString(hash[:])}
		text := string(data)
		if !utf8.Valid(data) {
			text = strings.ToValidUTF8(text, "\ufffd")
			out.Issues = append(out.Issues, evidence.Path+": 非UTF-8字节已替换，仅作线索")
		}
		appendFrontendCandidates(&out, seenCandidates, text, base, evidence)
		if strings.EqualFold(filepath.Ext(rel), ".map") {
			var sourceMap struct {
				Sources        []string `json:"sources"`
				SourcesContent []string `json:"sourcesContent"`
			}
			if json.Unmarshal(data, &sourceMap) != nil {
				out.Issues = append(out.Issues, evidence.Path+": sourcemap格式无效")
			} else {
				for i, content := range sourceMap.SourcesContent {
					if err := ctx.Err(); err != nil {
						return out, err
					}
					if i >= maxFrontendFiles {
						out.Truncated = true
						break
					}
					embedded := evidence
					embedded.Path += fmt.Sprintf("#sourcesContent[%d]", i)
					appendFrontendCandidates(&out, seenCandidates, content, base, embedded)
				}
			}
		}
	}
	return out, nil
}

func appendFrontendCandidates(out *frontendInventory, seen map[string]int, text string, base *url.URL, source frontendEvidence) {
	newlines := make([]int, 0)
	for offset, char := range text {
		if char == '\n' {
			newlines = append(newlines, offset)
		}
	}
	add := func(kind, reference, valueHash string, offset int) {
		if len(reference) > 1024 {
			out.Truncated = true
			return
		}
		item := frontendCandidate{Kind: kind, Reference: reference, ValueSHA256: valueHash, Occurrences: 1, Template: strings.Contains(reference, "${")}
		if kind != "sensitive-key" {
			ref, err := url.Parse(reference)
			if err != nil {
				return
			}
			resolved := base.ResolveReference(ref)
			if resolved.Scheme != "http" && resolved.Scheme != "https" {
				return
			}
			if resolved.User != nil {
				return
			}
			item.URL, item.SameOrigin = resolved.String(), frontendSameOrigin(base, resolved)
		}
		key := kind + "\x00" + item.URL
		if kind == "sensitive-key" {
			key += reference + valueHash
		}
		evidence := source
		evidence.Line = sort.SearchInts(newlines, offset) + 1
		if index, ok := seen[key]; ok {
			previous := &out.Candidates[index]
			previous.Occurrences++
			if len(previous.Evidence) < 4 {
				previous.Evidence = append(previous.Evidence, evidence)
			}
			return
		}
		if len(out.Candidates) >= maxFrontendCandidates {
			out.Truncated = true
			return
		}
		item.Evidence = []frontendEvidence{evidence}
		seen[key] = len(out.Candidates)
		out.Candidates = append(out.Candidates, item)
	}
	const maxLiterals = 4096
	matches := frontendQuoted.FindAllStringSubmatchIndex(text, maxLiterals+1)
	if len(matches) > maxLiterals {
		matches = matches[:maxLiterals]
		out.Truncated = true
	}
	for _, match := range matches {
		literal := frontendLiteral(text[match[2]:match[3]])
		if !(strings.HasPrefix(literal, "/") || strings.HasPrefix(literal, "http://") || strings.HasPrefix(literal, "https://") || frontendAsset.MatchString(literal)) {
			continue
		}
		kind := "route"
		if frontendAsset.MatchString(literal) {
			kind = "resource"
		} else if frontendAPI.MatchString(literal) {
			kind = "api"
		} else if strings.HasPrefix(literal, "http") {
			kind = "url"
		}
		add(kind, literal, "", match[2])
	}
	for _, match := range frontendMap.FindAllStringSubmatchIndex(text, 64) {
		add("sourcemap", text[match[2]:match[3]], "", match[2])
	}
	for _, match := range frontendConfig.FindAllStringSubmatchIndex(text, 128) {
		key, value := text[match[2]:match[3]], frontendLiteral(text[match[4]:match[5]])
		lower := strings.ToLower(key)
		if strings.Contains(lower, "key") || strings.Contains(lower, "token") || strings.Contains(lower, "secret") {
			if frontendPlaceholder(value) {
				continue
			}
			hash := sha256.Sum256([]byte(value))
			add("sensitive-key", key, hex.EncodeToString(hash[:]), match[2])
		} else {
			add("config", value, "", match[4])
		}
	}
}

func frontendLiteral(raw string) string {
	value, err := strconv.Unquote(`"` + strings.ReplaceAll(strings.ReplaceAll(raw, `\/`, `/`), `"`, `\"`) + `"`)
	if err == nil {
		return value
	}
	return raw
}

func frontendPlaceholder(value string) bool {
	if len(value) < 12 {
		return true
	}
	lower := strings.ToLower(value)
	for _, marker := range []string{"your_", "example", "placeholder", "changeme", "dummy", "undefined", "test-token"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return strings.HasPrefix(lower, "<") || strings.HasPrefix(lower, "${")
}

func frontendSameOrigin(a, b *url.URL) bool {
	port := func(u *url.URL) string {
		if p := u.Port(); p != "" {
			return p
		}
		if u.Scheme == "https" {
			return "443"
		}
		return "80"
	}
	return strings.EqualFold(a.Scheme, b.Scheme) && strings.EqualFold(a.Hostname(), b.Hostname()) && port(a) == port(b)
}
