package cli

import (
	"fmt"
	"io"
	"log/slog"
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
func buildSinks(cfg config.Config, stdout io.Writer, m *observe.Metrics, log *slog.Logger) (result.Sink, func() error, error) {
	// onRetry drives the retry counter without result importing observe.
	// A nil Metrics (scan mode) yields a nil callback, which the sink
	// treats as "do not count" rather than panicking.
	onRetry := func(sinkType string) func() {
		if m == nil {
			return nil
		}
		return func() { m.SinkRetriesTotal.WithLabelValues(sinkType).Inc() }
	}

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

	unconditional := 0
	for i, sc := range cfg.Output.Sinks {
		// The format is resolved before the transport so an unknown name
		// is a boot failure with the same shape as an unknown type.
		fmtr, err := result.FormatterFor(sc.Format)
		if err != nil {
			_ = closeAll()
			return nil, nil, fmt.Errorf("output.sinks[%d]: %w", i, err)
		}
		if sc.Predicate.IsEmpty() {
			unconditional++
		}

		var s result.Sink
		name := strings.ToLower(strings.TrimSpace(sc.Type))
		switch name {
		case "stdout":
			s = result.NewJSONLSink(stdout)
		case "file":
			fs, err := result.NewFileSink(sc.Path)
			if err != nil {
				_ = closeAll()
				return nil, nil, fmt.Errorf("output.sinks[%d]: %w", i, err)
			}
			s = fs
			closers = append(closers, fs.Close)
		case "webhook":
			// The label, not the URL, is what reaches logs: a Discord
			// webhook URL carries its token in the path.
			label := fmt.Sprintf("webhook[%d]", i)
			s = result.NewWebhookSink(sc.URL, result.WebhookOptions{
				Timeout:     sc.Timeout,
				MaxAttempts: sc.MaxAttempts,
				Formatter:   fmtr,
				Logger:      log,
				Name:        label,
				OnRetry:     onRetry("webhook"),
			})
		default:
			_ = closeAll()
			return nil, nil, fmt.Errorf("output.sinks[%d]: unknown sink type %q", i, sc.Type)
		}

		// Wrapping is skipped for an unconditional sink so the delivery
		// path of an existing config is byte-for-byte what it was.
		if !sc.Predicate.IsEmpty() {
			s = result.NewRoutedSink(s, sc.Predicate)
			// The metric label distinguishes routed destinations, or an
			// operator debugging an outage cannot tell which of two
			// webhooks failed.
			name += "/" + fmtr.Name()
		}
		sinks = append(sinks, s)
		names = append(names, name)
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

	// Same checkpoint, one step further: a predicate only ever NARROWS
	// delivery, so a config in which every sink is predicated silently
	// drops any envelope matching none of them. config.validateOutput
	// enforces this too, but buildSinks is reachable with a
	// directly-constructed Config.
	if unconditional == 0 && !cfg.Output.AllowUnrouted {
		_ = closeAll()
		return nil, nil, fmt.Errorf("output.sinks: every sink carries a predicate, so an unmatched result would be emitted nowhere; give one sink no predicate, or set output.allow_unrouted")
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
