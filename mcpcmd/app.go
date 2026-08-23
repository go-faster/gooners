package mcpcmd

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strings"

	"github.com/go-faster/errors"
	"github.com/go-faster/sdk/app"
	"go.uber.org/zap"
	"go.uber.org/zap/exp/zapslog"
	"go.uber.org/zap/zapcore"
)

// AppOptions configures [Run].
type AppOptions struct {
	// Name is the service name reported to OpenTelemetry.
	Name string
	// Logging configures the logger the callback receives, and stays the
	// authority over it: an MCP server is launched by a client that passes
	// flags, not by an operator exporting environment variables.
	Logging LoggingFlags
	// Transport is what the server speaks. On stdio it decides whether
	// anything may be written to stdout at all; see [Run].
	Transport TransportFlags
}

// Run configures logging and OpenTelemetry from opts and calls fn.
//
// The telemetry providers are installed globally, which is what makes the
// clients built by internal/effect emit anything: they default to the global
// providers rather than taking them from every call site.
//
// It does not return: fn's error is reported and the process exits non-zero.
func Run(opts AppOptions, fn func(ctx context.Context, lg *slog.Logger, t *app.Telemetry) error) {
	zapCfg, err := opts.Logging.zapConfig()
	if err != nil {
		fatal(err)
	}
	// Exporters are checked before anything starts, so a misconfigured stdio
	// server fails at launch with a legible reason rather than emitting one
	// line of telemetry into the protocol stream.
	if err := checkStdout(opts); err != nil {
		fatal(err)
	}
	silenceUnconfiguredExporters()

	app.Run(func(ctx context.Context, lg *zap.Logger, t *app.Telemetry) error {
		slogger := slog.New(zapslog.NewHandler(lg.Core()))
		slog.SetDefault(slogger)
		return fn(ctx, slogger, t)
	},
		app.WithServiceName(opts.Name),
		app.WithZapConfig(zapCfg),
	)
}

// fatal reports a configuration error the operator can act on. It prints the
// message alone: a mistyped flag or environment variable deserves the accepted
// values, not a stack trace.
func fatal(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}

// signals are the OpenTelemetry signals these binaries emit, by the infix the
// environment variables selecting an exporter and an endpoint share.
var signals = []string{"TRACES", "METRICS", "LOGS"}

// stdoutSignals are the exporter selections that would write to stdout.
var stdoutSignals = []string{"OTEL_TRACES_EXPORTER", "OTEL_METRICS_EXPORTER", "OTEL_LOGS_EXPORTER"}

// silenceUnconfiguredExporters turns a signal off when nothing says where to
// send it.
//
// go-faster/sdk defaults every signal to OTLP on localhost:4317, so a server
// started where no collector runs retries a refused connection for its whole
// life and then stalls for seconds on shutdown flushing it. A daemon is
// configured once by an operator; an MCP server is launched per session by a
// client that knows nothing about collectors, so it would pay that on every
// start. Telemetry is therefore opt-in: naming an exporter or an endpoint is
// what turns it on.
func silenceUnconfiguredExporters() {
	for _, signal := range signals {
		exporter := "OTEL_" + signal + "_EXPORTER"
		if os.Getenv(exporter) != "" {
			continue
		}
		if slices.ContainsFunc([]string{
			"OTEL_EXPORTER_OTLP_ENDPOINT",
			"OTEL_EXPORTER_OTLP_" + signal + "_ENDPOINT",
			"GOFASTER_OTLP_ENDPOINTS",
			"GOFASTER_OTLP_" + signal + "_ENDPOINTS",
		}, func(name string) bool { return os.Getenv(name) != "" }) {
			continue
		}
		_ = os.Setenv(exporter, "none")
	}
}

// checkStdout refuses a configuration that would write anything but JSON-RPC
// frames to stdout.
//
// The stdio transport *is* stdout: a single line of telemetry or logging there
// desynchronizes the framing, and the client sees a broken server with no hint
// as to why. Rerouting it to stderr would silently ignore a configuration the
// operator meant, so this is an error instead.
func checkStdout(opts AppOptions) error {
	if opts.Transport.Transport != "stdio" {
		return nil
	}
	for _, name := range stdoutSignals {
		for exporter := range strings.SplitSeq(os.Getenv(name), ",") {
			if strings.EqualFold(strings.TrimSpace(exporter), "stdout") {
				return errors.Errorf(
					"%s=stdout writes telemetry to stdout, which is the MCP session on the stdio transport: use stderr, otlp or none",
					name)
			}
		}
	}
	if isStdout(opts.Logging.LogFile) {
		return errors.Errorf(
			"-log-file=%s writes logs to stdout, which is the MCP session on the stdio transport: leave it empty for stderr, or name a file",
			opts.Logging.LogFile)
	}
	return nil
}

// isStdout reports whether zap would treat path as standard output. "stdout"
// is a sink zap registers itself, so it never reaches the filesystem.
func isStdout(path string) bool {
	switch path {
	case "stdout", "/dev/stdout", "/dev/fd/1", "/proc/self/fd/1":
		return true
	default:
		return false
	}
}

// zapConfig translates the logging flags, which are the interface an MCP client
// drives, into the configuration go-faster/sdk/app builds its logger from.
func (flags *LoggingFlags) zapConfig() (zap.Config, error) {
	cfg := zap.NewProductionConfig()
	switch flags.LogFormat {
	case "json":
		cfg.Encoding = "json"
	case "text", "":
		cfg.Encoding = "console"
		cfg.EncoderConfig = zap.NewDevelopmentEncoderConfig()
	default:
		return cfg, errors.Errorf("unknown log format: %q", flags.LogFormat)
	}

	cfg.Level = zap.NewAtomicLevelAt(zapLevel(flags.LogLevel))
	cfg.OutputPaths = []string{"stderr"}
	if flags.LogFile != "" {
		cfg.OutputPaths = []string{flags.LogFile}
	}
	cfg.ErrorOutputPaths = []string{"stderr"}
	return cfg, nil
}

func zapLevel(level slog.Level) zapcore.Level {
	switch {
	case level <= slog.LevelDebug:
		return zapcore.DebugLevel
	case level <= slog.LevelInfo:
		return zapcore.InfoLevel
	case level <= slog.LevelWarn:
		return zapcore.WarnLevel
	default:
		return zapcore.ErrorLevel
	}
}
