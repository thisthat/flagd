package otel

import (
	"context"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/metric"
	semconv "go.opentelemetry.io/otel/semconv/v1.13.0"
	"reflect"
	"testing"
)

const svcName = "mySvc"

func TestHTTPAttributes(t *testing.T) {
	type HTTPReqProperties struct {
		Service string
		ID      string
		Method  string
		Code    string
	}

	tests := []struct {
		name string
		req  HTTPReqProperties
		want []attribute.KeyValue
	}{
		{
			name: "empty attributes",
			req: HTTPReqProperties{
				Service: "",
				ID:      "",
				Method:  "",
				Code:    "",
			},
			want: []attribute.KeyValue{
				semconv.ServiceNameKey.String(""),
				semconv.HTTPURLKey.String(""),
				semconv.HTTPMethodKey.String(""),
				semconv.HTTPStatusCodeKey.String(""),
			},
		},
		{
			name: "some values",
			req: HTTPReqProperties{
				Service: "myService",
				ID:      "#123",
				Method:  "POST",
				Code:    "300",
			},
			want: []attribute.KeyValue{
				semconv.ServiceNameKey.String("myService"),
				semconv.HTTPURLKey.String("#123"),
				semconv.HTTPMethodKey.String("POST"),
				semconv.HTTPStatusCodeKey.String("300"),
			},
		},
		{
			name: "special chars",
			req: HTTPReqProperties{
				Service: "!@#$%^&*()_+|}{[];',./<>",
				ID:      "",
				Method:  "",
				Code:    "",
			},
			want: []attribute.KeyValue{
				semconv.ServiceNameKey.String("!@#$%^&*()_+|}{[];',./<>"),
				semconv.HTTPURLKey.String(""),
				semconv.HTTPMethodKey.String(""),
				semconv.HTTPStatusCodeKey.String(""),
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := MetricsRecorder{}
			res := rec.HttpAttributes(tt.req.Service, tt.req.ID, tt.req.Method, tt.req.Code)
			if len(res) != 4 {
				t.Errorf("OTelMetricsRecorder.setAttributes() must provide 4 attributes")
			}
			for i := 0; i < 4; i++ {
				if !reflect.DeepEqual(res[i], tt.want[i]) {
					t.Errorf("attribute %d = %v, want %v", i, res[i], tt.want[i])
				}
			}
		})
	}
}

func TestNewOTelRecorder(t *testing.T) {
	exp := metric.NewManualReader()
	rec := NewOTelRecorder(exp, svcName)
	if rec == nil {
		t.Errorf("Expected object to be created")
	}
	if rec.httpRequestDurHistogram == nil {
		t.Errorf("Expected httpRequestDurHistogram to be created")
	}
	if rec.httpResponseSizeHistogram == nil {
		t.Errorf("Expected httpResponseSizeHistogram to be created")
	}
	if rec.httpRequestsInflight == nil {
		t.Errorf("Expected httpRequestsInflight to be created")
	}
	if rec.impressions == nil {
		t.Errorf("Expected impressions to be created")
	}
}

func TestMetrics(t *testing.T) {
	exp := metric.NewManualReader()
	rec := NewOTelRecorder(exp, svcName)
	ctx := context.TODO()
	attrs := []attribute.KeyValue{
		semconv.ServiceNameKey.String(svcName),
	}
	const n = 5
	type MetricF func()
	tests := []struct {
		name       string
		metricFunc MetricF
	}{
		{
			name: "HttpRequestDuration",
			metricFunc: func() {
				for i := 0; i < n; i++ {
					rec.HttpRequestDuration(ctx, 10, attrs)
				}
			},
		},
		{
			name: "HttpResponseSize",
			metricFunc: func() {
				for i := 0; i < n; i++ {
					rec.HttpResponseSize(ctx, 100, attrs)
				}
			},
		},
		{
			name: "InFlightRequestStart",
			metricFunc: func() {
				for i := 0; i < n; i++ {
					rec.InFlightRequestStart(ctx, attrs)
					rec.InFlightRequestEnd(ctx, attrs)
				}
			},
		},
		{
			name: "Impressions",
			metricFunc: func() {
				for i := 0; i < n; i++ {
					rec.Impressions(ctx, "a", "b")
				}
			},
		},
	}
	i := 0
	for _, tt := range tests {
		i++
		tt.metricFunc()
		data, err := exp.Collect(context.TODO())
		if err != nil {
			t.Errorf("Got %v", err)
		}
		if len(data.ScopeMetrics) != 1 {
			t.Errorf("A single scope is expected, got %d", len(data.ScopeMetrics))
		}
		scopeMetrics := data.ScopeMetrics[0]
		if !reflect.DeepEqual(scopeMetrics.Scope.Name, svcName) {
			t.Errorf("Scope name %s, want %s", scopeMetrics.Scope.Name, svcName)
		}

		if len(scopeMetrics.Metrics) != i {
			t.Errorf("Expected %d metric point, got %d", i, len(scopeMetrics.Metrics))
		}

	}

}
