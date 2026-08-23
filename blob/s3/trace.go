package s3

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// tracerName identifies the instrumentation, not the service.
const tracerName = "github.com/go-faster/gooners/blob/s3"

// start begins a span for one store operation.
//
// The HTTP client underneath is traced too, but a span per request cannot say
// what the store was doing: an upload is one PutObject or several, plus the
// RemoveObject that undoes it when the object turns out to be too large. This
// span is the operation those requests belong to.
//
// Nothing derived from the payload goes on it. The id and size describe the
// object, while the file name and the declared type came from a tool and can
// hold anything a model produced.
func (s *Store) start(ctx context.Context, name string, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	return s.tracer.Start(ctx, name,
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(append([]attribute.KeyValue{
			attribute.String("aws.s3.bucket", s.bucket),
			attribute.String("blob.namespace", s.namespace),
		}, attrs...)...),
	)
}

// end closes a span, recording the error the operation returned. It takes the
// error by pointer so a deferred call sees the named return value.
func end(span trace.Span, err *error) {
	if *err != nil {
		span.RecordError(*err)
		span.SetStatus(codes.Error, (*err).Error())
	}
	span.End()
}
