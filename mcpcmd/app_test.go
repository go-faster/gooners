package mcpcmd

import (
	"log/slog"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zapcore"
)

func TestCheckStdout(t *testing.T) {
	tests := []struct {
		name      string
		transport string
		logFile   string
		env       map[string]string
		wantErr   string
	}{
		{name: "stdio with no telemetry configured", transport: "stdio"},
		{
			name:      "stdio refuses a stdout trace exporter",
			transport: "stdio",
			env:       map[string]string{"OTEL_TRACES_EXPORTER": "stdout"},
			wantErr:   "OTEL_TRACES_EXPORTER=stdout",
		},
		{
			name:      "stdio refuses stdout among several exporters",
			transport: "stdio",
			env:       map[string]string{"OTEL_METRICS_EXPORTER": "otlp, stdout"},
			wantErr:   "OTEL_METRICS_EXPORTER=stdout",
		},
		{
			name:      "stdio refuses a stdout log exporter",
			transport: "stdio",
			env:       map[string]string{"OTEL_LOGS_EXPORTER": "STDOUT"},
			wantErr:   "OTEL_LOGS_EXPORTER=stdout",
		},
		{
			name:      "stdio allows stderr and otlp",
			transport: "stdio",
			env:       map[string]string{"OTEL_TRACES_EXPORTER": "stderr", "OTEL_METRICS_EXPORTER": "otlp"},
		},
		{
			name:      "stdio refuses a log file on stdout",
			transport: "stdio",
			logFile:   "/dev/stdout",
			wantErr:   "-log-file=/dev/stdout",
		},
		{
			name:      "stdio allows a real log file",
			transport: "stdio",
			logFile:   "/var/log/ssh-mcp.log",
		},
		{
			// An HTTP transport owns neither stdout nor stderr, so a stdout
			// exporter there is a legitimate way to watch a server run.
			name:      "http allows stdout",
			transport: "streamable-http",
			logFile:   "/dev/stdout",
			env:       map[string]string{"OTEL_TRACES_EXPORTER": "stdout"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, name := range stdoutSignals {
				t.Setenv(name, tt.env[name])
			}

			err := checkStdout(AppOptions{
				Logging:   LoggingFlags{LogFile: tt.logFile},
				Transport: TransportFlags{Transport: tt.transport},
			})
			if tt.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tt.wantErr)
		})
	}
}

func TestZapConfig(t *testing.T) {
	tests := []struct {
		name        string
		flags       LoggingFlags
		wantEncode  string
		wantLevel   zapcore.Level
		wantOutputs []string
		wantErr     bool
	}{
		{
			name:        "defaults to console on stderr",
			flags:       LoggingFlags{LogLevel: slog.LevelInfo},
			wantEncode:  "console",
			wantLevel:   zapcore.InfoLevel,
			wantOutputs: []string{"stderr"},
		},
		{
			name:        "json format",
			flags:       LoggingFlags{LogFormat: "json", LogLevel: slog.LevelWarn},
			wantEncode:  "json",
			wantLevel:   zapcore.WarnLevel,
			wantOutputs: []string{"stderr"},
		},
		{
			name:        "log file",
			flags:       LoggingFlags{LogFormat: "text", LogLevel: slog.LevelDebug, LogFile: "/tmp/x.log"},
			wantEncode:  "console",
			wantLevel:   zapcore.DebugLevel,
			wantOutputs: []string{"/tmp/x.log"},
		},
		{
			name:        "error level",
			flags:       LoggingFlags{LogLevel: slog.LevelError},
			wantEncode:  "console",
			wantLevel:   zapcore.ErrorLevel,
			wantOutputs: []string{"stderr"},
		},
		{name: "unknown format", flags: LoggingFlags{LogFormat: "logfmt"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := tt.flags.zapConfig()
			if tt.wantErr {
				require.ErrorContains(t, err, "unknown log format")
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.wantEncode, cfg.Encoding)
			require.Equal(t, tt.wantLevel, cfg.Level.Level())
			require.Equal(t, tt.wantOutputs, cfg.OutputPaths)
			// Diagnostics from the logger itself must never land on stdout,
			// whatever the logs are configured to do.
			require.Equal(t, []string{"stderr"}, cfg.ErrorOutputPaths)
		})
	}
}

func TestSilenceUnconfiguredExporters(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want map[string]string
	}{
		{
			name: "nothing configured turns every signal off",
			want: map[string]string{
				"OTEL_TRACES_EXPORTER":  "none",
				"OTEL_METRICS_EXPORTER": "none",
				"OTEL_LOGS_EXPORTER":    "none",
			},
		},
		{
			name: "a shared endpoint leaves the defaults alone",
			env:  map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://otelcol:4317"},
			want: map[string]string{
				"OTEL_TRACES_EXPORTER":  "",
				"OTEL_METRICS_EXPORTER": "",
				"OTEL_LOGS_EXPORTER":    "",
			},
		},
		{
			name: "a per-signal endpoint only enables that signal",
			env:  map[string]string{"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT": "http://otelcol:4317"},
			want: map[string]string{
				"OTEL_TRACES_EXPORTER":  "",
				"OTEL_METRICS_EXPORTER": "none",
				"OTEL_LOGS_EXPORTER":    "none",
			},
		},
		{
			name: "an explicit exporter is never overridden",
			env:  map[string]string{"OTEL_METRICS_EXPORTER": "prometheus"},
			want: map[string]string{
				"OTEL_TRACES_EXPORTER":  "none",
				"OTEL_METRICS_EXPORTER": "prometheus",
				"OTEL_LOGS_EXPORTER":    "none",
			},
		},
		{
			name: "go-faster fan-out endpoints count as configuration",
			env:  map[string]string{"GOFASTER_OTLP_ENDPOINTS": "http://a:4317,http://b:4317"},
			want: map[string]string{
				"OTEL_TRACES_EXPORTER":  "",
				"OTEL_METRICS_EXPORTER": "",
				"OTEL_LOGS_EXPORTER":    "",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, name := range []string{
				"OTEL_TRACES_EXPORTER", "OTEL_METRICS_EXPORTER", "OTEL_LOGS_EXPORTER",
				"OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT",
				"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", "OTEL_EXPORTER_OTLP_LOGS_ENDPOINT",
				"GOFASTER_OTLP_ENDPOINTS", "GOFASTER_OTLP_TRACES_ENDPOINTS",
				"GOFASTER_OTLP_METRICS_ENDPOINTS", "GOFASTER_OTLP_LOGS_ENDPOINTS",
			} {
				t.Setenv(name, tt.env[name])
			}

			silenceUnconfiguredExporters()

			for name, want := range tt.want {
				require.Equal(t, want, os.Getenv(name), name)
			}
		})
	}
}
