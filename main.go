package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"time"

	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/log/global"
	"go.opentelemetry.io/otel/metric"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.24.0"
)

const serviceName = "hello-service"

func main() {
	if err := run(); err != nil {
		slog.Error("application failed", "error", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	res, err := newResource()
	if err != nil {
		return err
	}

	// Setup OpenTelemetry Providers
	shutdownTracer, err := setupTracerProvider(ctx, res)
	if err != nil {
		return err
	}
	defer func() {
		if err := shutdownTracer(context.Background()); err != nil {
			slog.Error("failed to shutdown TracerProvider", "error", err)
		}
	}()

	shutdownMeter, err := setupMeterProvider(ctx, res)
	if err != nil {
		return err
	}
	defer func() {
		if err := shutdownMeter(context.Background()); err != nil {
			slog.Error("failed to shutdown MeterProvider", "error", err)
		}
	}()

	shutdownLogger, err := setupLoggerProvider(ctx, res)
	if err != nil {
		return err
	}
	defer func() {
		if err := shutdownLogger(context.Background()); err != nil {
			slog.Error("failed to shutdown LoggerProvider", "error", err)
		}
	}()

	// Setup slog to use OpenTelemetry
	logger := otelslog.NewLogger(serviceName)
	slog.SetDefault(logger)

	// Create a custom metric
	meter := otel.Meter(serviceName)
	helloCounter, err := meter.Int64Counter(
		"hello.requests.count",
		metric.WithDescription("Number of hello requests received"),
	)
	if err != nil {
		return err
	}

	// Setup HTTP Server
	mux := http.NewServeMux()
	mux.HandleFunc("/hello", func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()

		// Add custom attributes to the span
		tracer := otel.Tracer(serviceName)
		ctx, span := tracer.Start(ctx, "hello-handler")
		defer span.End()

		span.SetAttributes(attribute.String("user", "demo"))

		// Increment metric
		helloCounter.Add(ctx, 1, metric.WithAttributes(attribute.String("endpoint", "/hello")))

		// Log using slog (which is now bridged to OTel, so it automatically gets TraceID/SpanID)
		slog.InfoContext(ctx, "Handling hello request", "user", "demo")

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("Hello, OpenTelemetry World!\n"))

	})

	// Wrap the mux with OpenTelemetry HTTP instrumentation
	handler := otelhttp.NewHandler(mux, "/")

	srv := &http.Server{
		Addr:         ":8080",
		BaseContext:  func(_ net.Listener) context.Context { return ctx },
		ReadTimeout:  time.Second,
		WriteTimeout: 10 * time.Second,
		Handler:      handler,
	}

	srvErr := make(chan error, 1)
	go func() {
		slog.Info("Starting server", "addr", srv.Addr)
		srvErr <- srv.ListenAndServe()
	}()

	// Wait for interruption.
	select {
	case err := <-srvErr:
		// Error when starting HTTP server.
		return err
	case <-ctx.Done():
		// Wait for first CTRL+C.
		// Stop receiving signal notifications as soon as possible.
		stop()
	}

	// When Shutdown is called, ListenAndServe immediately returns ErrServerClosed.
	err = srv.Shutdown(context.Background())
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}

	return nil
}

// newResource returns a resource describing this application.
func newResource() (*resource.Resource, error) {
	return resource.Merge(resource.Default(),
		resource.NewWithAttributes(resource.Default().SchemaURL(),
			semconv.ServiceName(serviceName),
			semconv.ServiceVersion("0.1.0"),
		))
}

func setupTracerProvider(ctx context.Context, res *resource.Resource) (func(context.Context) error, error) {
	exporter, err := otlptracegrpc.New(ctx, otlptracegrpc.WithInsecure())
	if err != nil {
		return nil, err
	}

	tracerProvider := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(tracerProvider)

	return tracerProvider.Shutdown, nil
}

func setupMeterProvider(ctx context.Context, res *resource.Resource) (func(context.Context) error, error) {
	exporter, err := otlpmetricgrpc.New(ctx, otlpmetricgrpc.WithInsecure())
	if err != nil {
		return nil, err
	}

	meterProvider := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exporter, sdkmetric.WithInterval(3*time.Second))),
		sdkmetric.WithResource(res),
	)
	otel.SetMeterProvider(meterProvider)

	return meterProvider.Shutdown, nil
}

func setupLoggerProvider(ctx context.Context, res *resource.Resource) (func(context.Context) error, error) {
	exporter, err := otlploggrpc.New(ctx, otlploggrpc.WithInsecure())
	if err != nil {
		return nil, err
	}

	loggerProvider := sdklog.NewLoggerProvider(
		sdklog.WithProcessor(sdklog.NewBatchProcessor(exporter)),
		sdklog.WithResource(res),
	)

	global.SetLoggerProvider(loggerProvider)
	return loggerProvider.Shutdown, nil
}
