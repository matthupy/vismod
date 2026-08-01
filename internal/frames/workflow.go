package frames

import (
	"fmt"
	"regexp"
	"strings"
	"text/template"

	"github.com/vismod/vismod/internal/config"
)

// Workflows are named, parameterized FFmpeg ARGUMENT-LIST templates —
// never shell strings. They are an operator-trust boundary: the
// guardrails below are the security contract (see SECURITY.md and
// docs/custom-ffmpeg-workflows.md).
//
// Guardrails enforced at validation time (boot and `vismod workflows
// validate`):
//  1. No shell — args are rendered per-element and passed to
//     exec.CommandContext directly.
//  2. Allowed placeholders only: {{.Input}}, {{.WorkDir}}, {{.MaxFrames}},
//     {{.MaxWidth}}. Unknown placeholders fail validation.
//  3. {{.Input}} is bound to the job's local file: exactly one -i, its
//     value exactly {{.Input}}, and {{.Input}} appears nowhere else.
//  4. Protocol allow-list: remote/indirect FFmpeg protocols (http, rtmp,
//     concat, pipe, subfile, data, tcp, udp, file:, ...) are rejected in
//     templates and rendered args — only plain local paths.
//  5. Output confinement: the output pattern (last arg) must live under
//     {{.WorkDir}}; no other absolute paths and no ".." traversal.

// TemplateValues are the ONLY substitutions available to workflows.
type TemplateValues struct {
	Input     string
	WorkDir   string
	MaxFrames int
	MaxWidth  int
}

var (
	placeholderRe = regexp.MustCompile(`{{\s*\.?([A-Za-z0-9_.]*)\s*}}`)

	allowedPlaceholders = map[string]bool{
		"Input": true, "WorkDir": true, "MaxFrames": true, "MaxWidth": true,
	}

	// forbiddenProtoRe rejects FFmpeg remote/indirect protocol prefixes.
	// A single letter before ":" is allowed (Windows drive paths).
	forbiddenProtoRe = regexp.MustCompile(`(?i)^(https?|rtmpt?e?s?|rtsp|concat|pipe|subfile|data|tcp|udp|rtp|srtp?|ftp|sftp|smb|gopher|crypto|cache|async|hls|tls|unix|fd|file)\s*:`)
)

// ValidateWorkflow checks one workflow template against the guardrails.
func ValidateWorkflow(name string, wf config.WorkflowConfig, cfg config.FFmpegConfig) error {
	if len(wf.Args) == 0 {
		return fmt.Errorf("workflow %q: empty args", name)
	}
	if cfg.MaxFrames <= 0 {
		return fmt.Errorf("workflow %q: ffmpeg.max_frames must be > 0 (hard cost/disk cap)", name)
	}

	inputCount, dashICount := 0, 0
	for i, arg := range wf.Args {
		// Placeholder allow-list.
		for _, m := range placeholderRe.FindAllStringSubmatch(arg, -1) {
			if !allowedPlaceholders[m[1]] {
				return fmt.Errorf("workflow %q: arg %d uses unknown placeholder {{.%s}} (allowed: Input, WorkDir, MaxFrames, MaxWidth)", name, i, m[1])
			}
		}
		if strings.Contains(arg, "{{.Input}}") {
			inputCount++
			if arg != "{{.Input}}" {
				return fmt.Errorf("workflow %q: {{.Input}} must be a standalone argument, not embedded in %q", name, arg)
			}
			if i == 0 || wf.Args[i-1] != "-i" {
				return fmt.Errorf("workflow %q: {{.Input}} may only appear immediately after -i", name)
			}
		}
		if arg == "-i" {
			dashICount++
		}
		if err := checkForbidden(name, i, arg); err != nil {
			return err
		}
	}
	if dashICount != 1 || inputCount != 1 {
		return fmt.Errorf("workflow %q: exactly one \"-i\" \"{{.Input}}\" pair is required (found %d -i, %d {{.Input}}); users cannot inject a second input", name, dashICount, inputCount)
	}

	// Output confinement: the last arg is the output pattern and must be
	// rooted in the pipeline-owned WorkDir.
	out := wf.Args[len(wf.Args)-1]
	if !strings.HasPrefix(out, "{{.WorkDir}}/") && !strings.HasPrefix(out, "{{.WorkDir}}\\") {
		return fmt.Errorf("workflow %q: output %q must live under {{.WorkDir}} (output confinement)", name, out)
	}
	// No other arg may reference an absolute path or WorkDir escape.
	for i, arg := range wf.Args[:len(wf.Args)-1] {
		if arg == "{{.Input}}" {
			continue
		}
		if strings.Contains(arg, "{{.WorkDir}}") {
			return fmt.Errorf("workflow %q: arg %d: {{.WorkDir}} is only permitted in the final output argument", name, i)
		}
		if looksAbsolutePath(arg) {
			return fmt.Errorf("workflow %q: arg %d (%q): absolute paths other than {{.Input}}/{{.WorkDir}} are forbidden", name, i, arg)
		}
	}

	// Dry-render against synthetic values proves the template renders and
	// its output stays inside the WorkDir.
	rendered, err := RenderWorkflow(wf, TemplateValues{
		Input:     "/synthetic/input.mp4",
		WorkDir:   "/synthetic/workdir",
		MaxFrames: cfg.MaxFrames,
		MaxWidth:  cfg.MaxWidth,
	})
	if err != nil {
		return fmt.Errorf("workflow %q: dry-render: %w", name, err)
	}
	for i, arg := range rendered {
		if err := checkForbidden(name, i, arg); err != nil {
			return err
		}
		if strings.Contains(arg, "..") {
			return fmt.Errorf("workflow %q: rendered arg %d (%q) contains path traversal", name, i, arg)
		}
	}
	if !strings.HasPrefix(rendered[len(rendered)-1], "/synthetic/workdir") {
		return fmt.Errorf("workflow %q: rendered output escapes the WorkDir", name)
	}
	return nil
}

// forbiddenProtoNames complements forbiddenProtoRe for FFmpeg protocols
// that take comma-separated options BEFORE the colon (e.g.
// "subfile,,start,0,end,0:/etc/passwd") and for protocol chaining
// ("cache:http://..."). The head of the arg (everything before the first
// colon) is tokenized on commas and each token checked.
var forbiddenProtoNames = map[string]bool{
	"http": true, "https": true, "rtmp": true, "rtmpe": true, "rtmps": true,
	"rtmpt": true, "rtmpte": true, "rtmpts": true, "rtsp": true,
	"concat": true, "concatf": true, "pipe": true, "subfile": true,
	"data": true, "tcp": true, "udp": true, "rtp": true, "srt": true,
	"srtp": true, "ftp": true, "sftp": true, "smb": true, "gopher": true,
	"gophers": true, "crypto": true, "cache": true, "async": true,
	"hls": true, "tls": true, "unix": true, "fd": true, "file": true,
	"ipfs": true, "ipns": true, "amqp": true, "zmq": true, "icecast": true,
}

func checkForbidden(name string, i int, arg string) error {
	reject := func() error {
		return fmt.Errorf("workflow %q: arg %d (%q) references a forbidden protocol — only plain local paths are permitted (SSRF/exfiltration guard)", name, i, arg)
	}
	if forbiddenProtoRe.MatchString(arg) || strings.Contains(arg, "://") {
		return reject()
	}
	if idx := strings.Index(arg, ":"); idx > 1 { // idx==1 is a Windows drive letter
		head := strings.ToLower(arg[:idx])
		for tok := range strings.SplitSeq(head, ",") {
			if forbiddenProtoNames[strings.TrimSpace(tok)] {
				return reject()
			}
		}
	}
	return nil
}

func looksAbsolutePath(arg string) bool {
	if strings.HasPrefix(arg, "/") {
		return true
	}
	// Windows drive letter (C:\ or C:/).
	if len(arg) >= 3 && arg[1] == ':' && (arg[2] == '\\' || arg[2] == '/') {
		return true
	}
	return false
}

// ValidateAll validates every configured workflow plus the default
// selection. Used at boot and by `vismod workflows validate`.
func ValidateAll(cfg config.FFmpegConfig) error {
	if len(cfg.Workflows) == 0 {
		return fmt.Errorf("no ffmpeg workflows configured")
	}
	if _, ok := cfg.Workflows[cfg.DefaultWorkflow]; !ok {
		return fmt.Errorf("default_workflow %q is not defined (have: %v)", cfg.DefaultWorkflow, workflowNames(cfg))
	}
	for name, wf := range cfg.Workflows {
		if err := ValidateWorkflow(name, wf, cfg); err != nil {
			return err
		}
	}
	return nil
}

func workflowNames(cfg config.FFmpegConfig) []string {
	names := make([]string, 0, len(cfg.Workflows))
	for n := range cfg.Workflows {
		names = append(names, n)
	}
	return names
}

// RenderWorkflow renders each arg through text/template with typed
// substitution. missingkey=error double-guards the placeholder allow-list.
func RenderWorkflow(wf config.WorkflowConfig, v TemplateValues) ([]string, error) {
	out := make([]string, 0, len(wf.Args))
	for i, arg := range wf.Args {
		t, err := template.New(fmt.Sprintf("arg%d", i)).Option("missingkey=error").Parse(arg)
		if err != nil {
			return nil, fmt.Errorf("arg %d (%q): %w", i, arg, err)
		}
		var b strings.Builder
		if err := t.Execute(&b, v); err != nil {
			return nil, fmt.Errorf("arg %d (%q): %w", i, arg, err)
		}
		out = append(out, b.String())
	}
	// -nostdin is mandatory (no tty interaction); prepend when absent.
	for _, a := range out {
		if a == "-nostdin" {
			return out, nil
		}
	}
	return append([]string{"-nostdin"}, out...), nil
}

