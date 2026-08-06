package gateway

import (
	"context"

	"github.com/go-faster/errors"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

// Reloadable is the reload target of a [Reloader], satisfied by [*Gateway].
type Reloadable interface {
	Reload(ctx context.Context, cfg *Config) (ReloadResult, error)
}

// Reloader drives a [Reloadable] from a [Source] and reports every attempt.
// It owns the operational concerns — when to reload, what to log, what to
// count — so the gateway itself only has to know how to apply a config.
type Reloader struct {
	source Source
	target Reloadable
	logger *zap.Logger
	onDone func(ReloadResult, error)
	total  metric.Int64Counter
}

// ReloaderOptions configures [NewReloader].
type ReloaderOptions struct {
	// Source and Target are required.
	Source Source
	Target Reloadable

	Logger        *zap.Logger
	MeterProvider metric.MeterProvider
	// OnReload, if set, is called after every attempt, including failed ones.
	OnReload func(ReloadResult, error)
}

func (o *ReloaderOptions) setDefaults() {
	if o.Logger == nil {
		o.Logger = zap.L()
	}
	if o.MeterProvider == nil {
		o.MeterProvider = otel.GetMeterProvider()
	}
	if o.OnReload == nil {
		o.OnReload = func(ReloadResult, error) {}
	}
}

// NewReloader returns a Reloader wiring opts.Source to opts.Target.
func NewReloader(opts ReloaderOptions) (*Reloader, error) {
	opts.setDefaults()
	if opts.Source == nil {
		return nil, errors.New("source is required")
	}
	if opts.Target == nil {
		return nil, errors.New("target is required")
	}
	total, err := opts.MeterProvider.Meter("mcpgateway").Int64Counter(
		"mcpgateway.config.reload",
		metric.WithDescription("Configuration reload attempts, by result."),
	)
	if err != nil {
		return nil, errors.Wrap(err, "create reload counter")
	}
	return &Reloader{
		source: opts.Source,
		target: opts.Target,
		logger: opts.Logger.With(zap.String("component", "reloader")),
		onDone: opts.OnReload,
		total:  total,
	}, nil
}

// Run watches the source and reloads the target on every reported change. It
// blocks until ctx is done and never fails the process over a bad config: a
// configuration that does not load or does not apply is logged and counted, and
// the target keeps running on the one it already has.
func (r *Reloader) Run(ctx context.Context) error {
	ch := make(chan struct{}, 1)
	grp, ctx := errgroup.WithContext(ctx)
	grp.Go(func() error {
		return r.source.Watch(ctx, ch)
	})
	grp.Go(func() error {
		for {
			select {
			case <-ctx.Done():
				return nil
			case <-ch:
				_, _ = r.Reload(ctx)
			}
		}
	})
	return grp.Wait()
}

// Reload performs one load-and-apply cycle and reports the outcome.
func (r *Reloader) Reload(ctx context.Context) (ReloadResult, error) {
	res, err := r.reload(ctx)
	r.observe(ctx, res, err)
	r.onDone(res, err)
	return res, err
}

func (r *Reloader) reload(ctx context.Context) (ReloadResult, error) {
	cfg, err := r.source.Load(ctx)
	if err != nil {
		return ReloadResult{}, errors.Wrap(err, "load config")
	}
	return r.target.Reload(ctx, cfg)
}

func (r *Reloader) observe(ctx context.Context, res ReloadResult, err error) {
	result := "success"
	if err != nil {
		result = "failure"
		r.logger.Error("config reload failed", zap.Error(err))
	} else {
		r.logger.Info("config reloaded",
			zap.Strings("added", res.Added),
			zap.Strings("removed", res.Removed),
			zap.Strings("restarted", res.Restarted),
			zap.Int("unchanged", len(res.Unchanged)),
		)
		if len(res.RestartRequired) > 0 {
			r.logger.Warn("config sections changed but need a process restart to take effect",
				zap.Strings("sections", res.RestartRequired))
		}
	}
	r.total.Add(ctx, 1, metric.WithAttributes(attribute.String("result", result)))
}
