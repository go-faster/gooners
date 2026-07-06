package alertmanager

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-openapi/strfmt"
	"github.com/prometheus/alertmanager/api/v2/models"
	"github.com/stretchr/testify/require"
)

func TestListAlerts_HappyPath(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/v2/alerts", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")

		// Write JSON directly to match what Alertmanager API returns
		jsonBody := `[
  {
    "labels": {"alertname": "HighErrorRate", "service": "checkout"},
    "annotations": {"summary": "error rate is high"},
    "generatorURL": "http://prom/graph",
    "fingerprint": "abc123",
    "startsAt": "2024-01-01T00:00:00Z",
    "endsAt": "0001-01-01T00:00:00Z",
    "updatedAt": "2024-01-01T00:00:00Z",
    "status": {"state": "active", "silencedBy": [], "inhibitedBy": [], "mutedBy": []},
    "receivers": [{"name": "default"}]
  },
  {
    "labels": {"alertname": "HighMemory", "service": "api"},
    "annotations": {"summary": "high memory usage"},
    "generatorURL": "http://prom/graph",
    "fingerprint": "def456",
    "startsAt": "2024-01-01T01:00:00Z",
    "endsAt": "0001-01-01T00:00:00Z",
    "updatedAt": "2024-01-01T01:00:00Z",
    "status": {"state": "suppressed", "silencedBy": ["silence-1"], "inhibitedBy": [], "mutedBy": []},
    "receivers": [{"name": "pager"}]
  }
]`
		w.Write([]byte(jsonBody))
	}))
	defer server.Close()

	client, err := NewClient(Config{AlertmanagerURL: server.URL})
	require.NoError(t, err)

	handler := listAlertsHandler(client)
	_, res, err := handler(context.Background(), nil, ListAlertsReq{})
	require.NoError(t, err)
	require.Equal(t, 2, res.Count)
	require.Len(t, res.Alerts, 2)

	// Check first alert
	require.Equal(t, "abc123", res.Alerts[0].Fingerprint)
	require.Equal(t, "HighErrorRate", res.Alerts[0].Labels["alertname"])
	require.Equal(t, "checkout", res.Alerts[0].Labels["service"])
	require.Equal(t, "error rate is high", res.Alerts[0].Annotations["summary"])
	require.Equal(t, "active", res.Alerts[0].State)
	require.Empty(t, res.Alerts[0].SilencedBy)
	require.Len(t, res.Alerts[0].Receivers, 1)
	require.Equal(t, "default", res.Alerts[0].Receivers[0])
	require.NotEmpty(t, res.Alerts[0].StartsAt)

	// Check second alert
	require.Equal(t, "def456", res.Alerts[1].Fingerprint)
	require.Equal(t, "HighMemory", res.Alerts[1].Labels["alertname"])
	require.Equal(t, "suppressed", res.Alerts[1].State)
	require.Len(t, res.Alerts[1].SilencedBy, 1)
	require.Equal(t, "silence-1", res.Alerts[1].SilencedBy[0])
}

func TestListAlerts_WithFilter(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/v2/alerts", r.URL.Path)
		// Verify that filter param is present
		require.True(t, len(r.URL.Query()["filter"]) > 0)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte("[]"))
	}))
	defer server.Close()

	client, err := NewClient(Config{AlertmanagerURL: server.URL})
	require.NoError(t, err)

	handler := listAlertsHandler(client)
	_, res, err := handler(context.Background(), nil, ListAlertsReq{Filter: `alertname="Foo"`})
	require.NoError(t, err)
	require.Equal(t, 0, res.Count)
}

func TestListAlerts_ServerError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"code":500,"message":"internal server error"}`))
	}))
	defer server.Close()

	client, err := NewClient(Config{AlertmanagerURL: server.URL})
	require.NoError(t, err)

	handler := listAlertsHandler(client)
	_, _, err = handler(context.Background(), nil, ListAlertsReq{})
	require.Error(t, err)
}

func TestListAlertGroups_HappyPath(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/v2/alerts/groups", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")

		jsonBody := `[
  {
    "labels": {"alertname": "HighErrorRate"},
    "receiver": {"name": "default"},
    "alerts": [
      {
        "labels": {"alertname": "HighErrorRate", "service": "checkout"},
        "annotations": {},
        "fingerprint": "alert1",
        "startsAt": "2024-01-01T00:00:00Z",
        "endsAt": "0001-01-01T00:00:00Z",
        "updatedAt": "2024-01-01T00:00:00Z",
        "status": {"state": "active", "silencedBy": [], "inhibitedBy": [], "mutedBy": []},
        "receivers": [{"name": "default"}]
      },
      {
        "labels": {"alertname": "HighErrorRate", "service": "api"},
        "annotations": {},
        "fingerprint": "alert2",
        "startsAt": "2024-01-01T00:00:00Z",
        "endsAt": "0001-01-01T00:00:00Z",
        "updatedAt": "2024-01-01T00:00:00Z",
        "status": {"state": "active", "silencedBy": [], "inhibitedBy": [], "mutedBy": []},
        "receivers": [{"name": "default"}]
      }
    ]
  }
]`
		w.Write([]byte(jsonBody))
	}))
	defer server.Close()

	client, err := NewClient(Config{AlertmanagerURL: server.URL})
	require.NoError(t, err)

	handler := listAlertGroupsHandler(client)
	_, res, err := handler(context.Background(), nil, ListAlertGroupsReq{})
	require.NoError(t, err)
	require.Equal(t, 1, res.Count)
	require.Len(t, res.Groups, 1)

	group := res.Groups[0]
	require.Equal(t, "HighErrorRate", group.Labels["alertname"])
	require.Equal(t, "default", group.Receiver)
	require.Len(t, group.Alerts, 2)
	require.Equal(t, "checkout", group.Alerts[0].Labels["service"])
	require.Equal(t, "api", group.Alerts[1].Labels["service"])
}

func TestAlertSummaryFromModel_AllFields(t *testing.T) {
	now := timePtr("2024-01-01T12:00:00Z")
	endTime := timePtr("2024-01-01T13:00:00Z")

	alert := &models.GettableAlert{
		Alert: models.Alert{
			Labels: models.LabelSet{
				"alertname": "TestAlert",
				"service":   "test",
			},
			GeneratorURL: "http://prometheus/graph",
		},
		Annotations: models.LabelSet{
			"summary":     "test summary",
			"description": "test description",
		},
		Fingerprint: new("fp123"),
		StartsAt:    now,
		EndsAt:      endTime,
		UpdatedAt:   now,
		Status: &models.AlertStatus{
			State:       new("active"),
			SilencedBy:  []string{"sil1", "sil2"},
			InhibitedBy: []string{"inhibit1"},
			MutedBy:     []string{"mute1"},
		},
		Receivers: []*models.ReceiverReference{
			{Name: new("receiver1")},
			{Name: new("receiver2")},
		},
	}

	summary := alertSummaryFromModel(alert)
	require.Equal(t, "fp123", summary.Fingerprint)
	require.Equal(t, "TestAlert", summary.Labels["alertname"])
	require.Equal(t, "test summary", summary.Annotations["summary"])
	require.Equal(t, "http://prometheus/graph", summary.GeneratorURL)
	require.Equal(t, "active", summary.State)
	require.Len(t, summary.SilencedBy, 2)
	require.Len(t, summary.InhibitedBy, 1)
	require.Len(t, summary.MutedBy, 1)
	require.Len(t, summary.Receivers, 2)
	require.Equal(t, "receiver1", summary.Receivers[0])
	require.NotEmpty(t, summary.StartsAt)
	require.NotEmpty(t, summary.EndsAt)
}

func TestAlertSummaryFromModel_NilFields(t *testing.T) {
	alert := &models.GettableAlert{
		Alert: models.Alert{
			Labels: models.LabelSet{"alertname": "Test"},
		},
		Annotations: models.LabelSet{},
	}

	summary := alertSummaryFromModel(alert)
	require.Equal(t, "Test", summary.Labels["alertname"])
	require.Empty(t, summary.Fingerprint)
	require.Empty(t, summary.State)
	require.Empty(t, summary.StartsAt)
	require.Empty(t, summary.EndsAt)
	require.Empty(t, summary.Receivers)
}

func TestParseFilterOption_Empty(t *testing.T) {
	filters, err := parseFilterOption("")
	require.NoError(t, err)
	require.Nil(t, filters)
}

func TestParseFilterOption_Valid(t *testing.T) {
	filters, err := parseFilterOption(`alertname="Foo",service="bar"`)
	require.NoError(t, err)
	require.Len(t, filters, 2)
	require.Equal(t, `alertname="Foo"`, filters[0])
	require.Equal(t, `service="bar"`, filters[1])
}

func TestParseFilterOption_Invalid(t *testing.T) {
	_, err := parseFilterOption("invalid syntax!")
	require.Error(t, err)
}

// Helper functions
func timePtr(s string) *strfmt.DateTime {
	t, _ := strfmt.ParseDateTime(s)
	return &t
}
