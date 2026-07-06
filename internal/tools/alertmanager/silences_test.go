package alertmanager

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/prometheus/alertmanager/api/v2/models"
	"github.com/stretchr/testify/require"
)

func TestListSilences_HappyPath(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/v2/silences", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")

		jsonBody := `[
  {
    "id": "11111111-1111-1111-1111-111111111111",
    "matchers": [
      {"name": "alertname", "value": "HighErrorRate", "isEqual": true, "isRegex": false}
    ],
    "startsAt": "2024-01-01T00:00:00Z",
    "endsAt": "2024-01-01T01:00:00Z",
    "createdBy": "alice",
    "comment": "maintenance window",
    "updatedAt": "2024-01-01T00:00:00Z",
    "status": {"state": "active"}
  }
]`
		w.Write([]byte(jsonBody))
	}))
	defer server.Close()

	client, err := NewClient(Config{AlertmanagerURL: server.URL})
	require.NoError(t, err)

	handler := listSilencesHandler(client)
	_, res, err := handler(context.Background(), nil, ListSilencesReq{})
	require.NoError(t, err)
	require.Equal(t, 1, res.Count)
	require.Len(t, res.Silences, 1)

	silence := res.Silences[0]
	require.Equal(t, "11111111-1111-1111-1111-111111111111", silence.ID)
	require.Equal(t, "active", silence.State)
	require.Equal(t, "alice", silence.CreatedBy)
	require.Equal(t, "maintenance window", silence.Comment)
	require.Len(t, silence.Matchers, 1)
	require.Equal(t, "alertname", silence.Matchers[0].Name)
}

func TestGetSilence_HappyPath(t *testing.T) {
	silenceID := "22222222-2222-2222-2222-222222222222"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/v2/silence/"+silenceID, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")

		jsonBody := `{
  "id": "22222222-2222-2222-2222-222222222222",
  "matchers": [
    {"name": "service", "value": "api", "isEqual": true, "isRegex": false}
  ],
  "startsAt": "2024-01-01T00:00:00Z",
  "endsAt": "2024-01-01T02:00:00Z",
  "createdBy": "bob",
  "comment": "debugging",
  "updatedAt": "2024-01-01T00:10:00Z",
  "status": {"state": "active"}
}`
		w.Write([]byte(jsonBody))
	}))
	defer server.Close()

	client, err := NewClient(Config{AlertmanagerURL: server.URL})
	require.NoError(t, err)

	handler := getSilenceHandler(client)
	_, res, err := handler(context.Background(), nil, GetSilenceReq{ID: silenceID})
	require.NoError(t, err)
	require.Equal(t, silenceID, res.ID)
	require.Equal(t, "bob", res.CreatedBy)
	require.Equal(t, "debugging", res.Comment)
}

func TestPreviewSilence_HappyPath(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		// Return alerts matching the matchers
		jsonBody := `[
  {
    "labels": {"alertname": "HighErrorRate", "service": "checkout"},
    "annotations": {},
    "fingerprint": "alert1",
    "startsAt": "2024-01-01T00:00:00Z",
    "endsAt": "0001-01-01T00:00:00Z",
    "updatedAt": "2024-01-01T00:00:00Z",
    "status": {"state": "active", "silencedBy": [], "inhibitedBy": [], "mutedBy": []},
    "receivers": []
  }
]`
		w.Write([]byte(jsonBody))
	}))
	defer server.Close()

	client, err := NewClient(Config{AlertmanagerURL: server.URL})
	require.NoError(t, err)

	handler := previewSilenceHandler(client)
	_, res, err := handler(context.Background(), nil, PreviewSilenceReq{
		Matchers: `alertname="HighErrorRate"`,
	})
	require.NoError(t, err)
	require.Equal(t, 1, res.Count)
	require.Len(t, res.Alerts, 1)
	require.Len(t, res.Matchers, 1)
	require.Equal(t, "alertname", res.Matchers[0].Name)
	require.Equal(t, "HighErrorRate", res.Matchers[0].Value)
}

// CreateSilence guardrail tests - these should NOT make network calls

func TestCreateSilence_GuardrailEmptyMatchers(t *testing.T) {
	failServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("unexpected network request")
	}))
	defer failServer.Close()

	client, err := NewClient(Config{AlertmanagerURL: failServer.URL})
	require.NoError(t, err)

	handler := createSilenceHandler(client)
	_, _, err = handler(context.Background(), nil, CreateSilenceReq{
		Matchers:  "",
		CreatedBy: "alice",
		Comment:   "test",
		Duration:  "1h",
	})
	require.Error(t, err)
}

func TestCreateSilence_GuardrailCatchAllOnly(t *testing.T) {
	failServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("unexpected network request")
	}))
	defer failServer.Close()

	client, err := NewClient(Config{AlertmanagerURL: failServer.URL})
	require.NoError(t, err)

	handler := createSilenceHandler(client)
	_, _, err = handler(context.Background(), nil, CreateSilenceReq{
		Matchers:  `service=~".*"`,
		CreatedBy: "alice",
		Comment:   "test",
		Duration:  "1h",
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "catch-all")
}

func TestCreateSilence_GuardrailMissingCreatedBy(t *testing.T) {
	failServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("unexpected network request")
	}))
	defer failServer.Close()

	client, err := NewClient(Config{AlertmanagerURL: failServer.URL})
	require.NoError(t, err)

	handler := createSilenceHandler(client)
	_, _, err = handler(context.Background(), nil, CreateSilenceReq{
		Matchers:  `alertname="Foo"`,
		CreatedBy: "",
		Comment:   "test",
		Duration:  "1h",
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "created_by")
}

func TestCreateSilence_GuardrailMissingComment(t *testing.T) {
	failServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("unexpected network request")
	}))
	defer failServer.Close()

	client, err := NewClient(Config{AlertmanagerURL: failServer.URL})
	require.NoError(t, err)

	handler := createSilenceHandler(client)
	_, _, err = handler(context.Background(), nil, CreateSilenceReq{
		Matchers:  `alertname="Foo"`,
		CreatedBy: "alice",
		Comment:   "   ",
		Duration:  "1h",
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "comment")
}

func TestCreateSilence_GuardrailBothEndsAtAndDuration(t *testing.T) {
	failServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("unexpected network request")
	}))
	defer failServer.Close()

	client, err := NewClient(Config{AlertmanagerURL: failServer.URL})
	require.NoError(t, err)

	handler := createSilenceHandler(client)
	now := time.Now().UTC().Format(time.RFC3339)
	endTime := time.Now().UTC().Add(1 * time.Hour).Format(time.RFC3339)
	_, _, err = handler(context.Background(), nil, CreateSilenceReq{
		Matchers:  `alertname="Foo"`,
		StartsAt:  now,
		EndsAt:    endTime,
		Duration:  "1h",
		CreatedBy: "alice",
		Comment:   "test",
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "exactly one")
}

func TestCreateSilence_GuardrailNeitherEndsAtNorDuration(t *testing.T) {
	failServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("unexpected network request")
	}))
	defer failServer.Close()

	client, err := NewClient(Config{AlertmanagerURL: failServer.URL})
	require.NoError(t, err)

	handler := createSilenceHandler(client)
	_, _, err = handler(context.Background(), nil, CreateSilenceReq{
		Matchers:  `alertname="Foo"`,
		CreatedBy: "alice",
		Comment:   "test",
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "exactly one")
}

func TestCreateSilence_GuardrailEndsAtBeforeStartsAt(t *testing.T) {
	failServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("unexpected network request")
	}))
	defer failServer.Close()

	client, err := NewClient(Config{AlertmanagerURL: failServer.URL})
	require.NoError(t, err)

	handler := createSilenceHandler(client)
	now := time.Now().UTC()
	_, _, err = handler(context.Background(), nil, CreateSilenceReq{
		Matchers:  `alertname="Foo"`,
		StartsAt:  now.Format(time.RFC3339),
		EndsAt:    now.Add(-1 * time.Hour).Format(time.RFC3339),
		CreatedBy: "alice",
		Comment:   "test",
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "after starts_at")
}

func TestCreateSilence_GuardrailExceedsMaxDuration(t *testing.T) {
	failServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("unexpected network request")
	}))
	defer failServer.Close()

	// Create client with small max duration
	client, err := NewClient(Config{
		AlertmanagerURL:    failServer.URL,
		MaxSilenceDuration: 1 * time.Hour,
	})
	require.NoError(t, err)

	handler := createSilenceHandler(client)
	_, _, err = handler(context.Background(), nil, CreateSilenceReq{
		Matchers:  `alertname="Foo"`,
		CreatedBy: "alice",
		Comment:   "test",
		Duration:  "2h",
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "exceeds")
}

// CreateSilence happy path test with full network interaction

func TestCreateSilence_HappyPath(t *testing.T) {
	silenceID := "33333333-3333-3333-3333-333333333333"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		if r.Method == "GET" && r.URL.Path == "/api/v2/alerts" {
			// Preview alerts
			alertsJSON := `[
  {
    "labels": {"alertname": "HighErrorRate", "service": "checkout"},
    "annotations": {},
    "fingerprint": "alert1",
    "startsAt": "2024-01-01T00:00:00Z",
    "endsAt": "0001-01-01T00:00:00Z",
    "updatedAt": "2024-01-01T00:00:00Z",
    "status": {"state": "active", "silencedBy": [], "inhibitedBy": [], "mutedBy": []},
    "receivers": []
  },
  {
    "labels": {"alertname": "HighErrorRate", "service": "api"},
    "annotations": {},
    "fingerprint": "alert2",
    "startsAt": "2024-01-01T00:00:00Z",
    "endsAt": "0001-01-01T00:00:00Z",
    "updatedAt": "2024-01-01T00:00:00Z",
    "status": {"state": "active", "silencedBy": [], "inhibitedBy": [], "mutedBy": []},
    "receivers": []
  }
]`
			w.Write([]byte(alertsJSON))
		} else if r.Method == "POST" && r.URL.Path == "/api/v2/silences" {
			// Create silence
			responseJSON := `{"silenceID": "33333333-3333-3333-3333-333333333333"}`
			w.Write([]byte(responseJSON))
		}
	}))
	defer server.Close()

	client, err := NewClient(Config{AlertmanagerURL: server.URL})
	require.NoError(t, err)

	handler := createSilenceHandler(client)
	now := time.Now().UTC()
	_, res, err := handler(context.Background(), nil, CreateSilenceReq{
		Matchers:  `alertname="HighErrorRate",service="checkout"`,
		StartsAt:  now.Format(time.RFC3339),
		Duration:  "1h",
		CreatedBy: "alice",
		Comment:   "debugging issue",
	})
	require.NoError(t, err)
	require.Equal(t, silenceID, res.ID)
	require.Equal(t, 2, res.MatchingAlerts)
	require.Len(t, res.Matchers, 2)
	require.NotEmpty(t, res.StartsAt)
	require.NotEmpty(t, res.EndsAt)
}

func TestExpireSilence_HappyPath(t *testing.T) {
	silenceID := "44444444-4444-4444-4444-444444444444"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/v2/silence/"+silenceID, r.URL.Path)
		require.Equal(t, "DELETE", r.Method)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client, err := NewClient(Config{AlertmanagerURL: server.URL})
	require.NoError(t, err)

	handler := expireSilenceHandler(client)
	_, res, err := handler(context.Background(), nil, ExpireSilenceReq{ID: silenceID})
	require.NoError(t, err)
	require.True(t, res.OK)
}

func TestExpireSilence_Error(t *testing.T) {
	silenceID := "55555555-5555-5555-5555-555555555555"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"code":500}`))
	}))
	defer server.Close()

	client, err := NewClient(Config{AlertmanagerURL: server.URL})
	require.NoError(t, err)

	handler := expireSilenceHandler(client)
	_, _, err = handler(context.Background(), nil, ExpireSilenceReq{ID: silenceID})
	require.Error(t, err)
}

func TestSilenceSummaryFromModel_AllFields(t *testing.T) {
	silence := &models.GettableSilence{
		Silence: models.Silence{
			Matchers: models.Matchers{
				{
					Name:    new("alertname"),
					Value:   new("TestAlert"),
					IsEqual: new(true),
					IsRegex: new(false),
				},
			},
			StartsAt:  timePtr("2024-01-01T00:00:00Z"),
			EndsAt:    timePtr("2024-01-01T01:00:00Z"),
			CreatedBy: new("charlie"),
			Comment:   new("test silence"),
		},
		ID:        new("66666666-6666-6666-6666-666666666666"),
		UpdatedAt: timePtr("2024-01-01T00:05:00Z"),
		Status: &models.SilenceStatus{
			State: new("active"),
		},
	}

	summary := silenceSummaryFromModel(silence)
	require.Equal(t, "66666666-6666-6666-6666-666666666666", summary.ID)
	require.Equal(t, "active", summary.State)
	require.Equal(t, "charlie", summary.CreatedBy)
	require.Equal(t, "test silence", summary.Comment)
	require.Len(t, summary.Matchers, 1)
	require.Equal(t, "alertname", summary.Matchers[0].Name)
	require.NotEmpty(t, summary.StartsAt)
	require.NotEmpty(t, summary.EndsAt)
	require.NotEmpty(t, summary.UpdatedAt)
}
