package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/vismod/vismod/internal/config"
	"github.com/vismod/vismod/internal/observe"
	"github.com/vismod/vismod/internal/result"
)

// buildSinks turns output.sinks into the single Sink the pipeline holds.
//
// Every sink is constructed eagerly so a bad path or URL is a BOOT
// failure, never a surprise on the first verdict. If any sink fails to
// construct, the ones already built are closed before returning.
//
// The returned close func must be deferred by the caller.
func buildSinks(cfg config.Config, stdout io.Writer, m *observe.Metrics) (result.Sink, func() error, error) {
	sinks := make([]result.Sink, 0, len(cfg.Output.Sinks))
	names := make([]string, 0, len(cfg.Output.Sinks))
	closers := make([]func() error, 0, len(cfg.Output.Sinks))

	closeAll := func() error {
		var firstErr error
		for _, c := range closers {
			if err := c(); err != nil && firstErr == nil {
				firstErr = err
			}
		}
		return firstErr
	}

	for i, sc := range cfg.Output.Sinks {
		switch strings.ToLower(strings.TrimSpace(sc.Type)) {
		case "stdout":
			sinks = append(sinks, result.NewJSONLSink(stdout))
			names = append(names, "stdout")
		case "file":
			fs, err := result.NewFileSink(sc.Path)
			if err != nil {
				_ = closeAll()
				return nil, nil, fmt.Errorf("output.sinks[%d]: %w", i, err)
			}
			sinks = append(sinks, fs)
			names = append(names, "file")
			closers = append(closers, fs.Close)
		case "webhook":
			sinks = append(sinks, result.NewWebhookSink(sc.URL, sc.Timeout, sc.MaxAttempts))
			names = append(names, "webhook")
		default:
			_ = closeAll()
			return nil, nil, fmt.Errorf("output.sinks[%d]: unknown sink type %q", i, sc.Type)
		}
	}

	// Last checkpoint before the destination is fixed for the process
	// lifetime. config.Validate normally catches an empty list, but
	// buildSinks is reachable with a directly-constructed Config, and a
	// MultiSink over zero sinks returns nil from Write — the pipeline
	// would Ack a job whose envelope went nowhere.
	if len(sinks) == 0 {
		_ = closeAll()
		return nil, nil, fmt.Errorf("output.sinks produced no sinks; results would go nowhere")
	}

	onFail := func(sinkType string) {
		if m != nil {
			m.SinkWriteFailuresTotal.WithLabelValues(sinkType).Inc()
		}
	}
	ms, err := result.NewMultiSink(sinks, names, onFail)
	if err != nil {
		_ = closeAll()
		return nil, nil, err
	}
	return ms, closeAll, nil
}
