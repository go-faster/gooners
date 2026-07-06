package alertmanager

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/prometheus/alertmanager/api/v2/models"
	"github.com/stretchr/testify/require"
)

func TestListReceivers_HappyPath(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/v2/receivers", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")

		receivers := []*models.Receiver{
			{
				Name: new("default"),
				Labels: models.LabelSet{
					"team": "devops",
				},
			},
			{
				Name: new("pager"),
				Labels: models.LabelSet{
					"team": "sre",
					"page": "true",
				},
			},
		}

		json.NewEncoder(w).Encode(receivers)
	}))
	defer server.Close()

	client, err := NewClient(Config{AlertmanagerURL: server.URL})
	require.NoError(t, err)

	handler := listReceiversHandler(client)
	_, res, err := handler(context.Background(), nil, ListReceiversReq{})
	require.NoError(t, err)
	require.Len(t, res.Receivers, 2)

	// Check first receiver
	require.Equal(t, "default", res.Receivers[0].Name)
	require.Equal(t, "devops", res.Receivers[0].Labels["team"])

	// Check second receiver
	require.Equal(t, "pager", res.Receivers[1].Name)
	require.Equal(t, "sre", res.Receivers[1].Labels["team"])
	require.Equal(t, "true", res.Receivers[1].Labels["page"])
}

func TestListReceivers_EmptyLabels(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`[{"name": "default", "labels": {}}]`))
	}))
	defer server.Close()

	client, err := NewClient(Config{AlertmanagerURL: server.URL})
	require.NoError(t, err)

	handler := listReceiversHandler(client)
	_, res, err := handler(context.Background(), nil, ListReceiversReq{})
	require.NoError(t, err)
	require.Len(t, res.Receivers, 1)
	require.Equal(t, "default", res.Receivers[0].Name)
	require.Empty(t, res.Receivers[0].Labels)
}

func TestListReceivers_NilLabels(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`[{"name": "default"}]`))
	}))
	defer server.Close()

	client, err := NewClient(Config{AlertmanagerURL: server.URL})
	require.NoError(t, err)

	handler := listReceiversHandler(client)
	_, res, err := handler(context.Background(), nil, ListReceiversReq{})
	require.NoError(t, err)
	require.Len(t, res.Receivers, 1)
	require.Equal(t, "default", res.Receivers[0].Name)
	require.Empty(t, res.Receivers[0].Labels)
}

func TestListReceivers_SkipsNilNames(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		receivers := []*models.Receiver{
			{
				Name: new("valid"),
			},
			{
				Name: nil,
			},
			{
				Name: new("another"),
			},
		}

		json.NewEncoder(w).Encode(receivers)
	}))
	defer server.Close()

	client, err := NewClient(Config{AlertmanagerURL: server.URL})
	require.NoError(t, err)

	handler := listReceiversHandler(client)
	_, res, err := handler(context.Background(), nil, ListReceiversReq{})
	require.NoError(t, err)
	// Should skip the nil-name receiver
	require.Len(t, res.Receivers, 2)
	require.Equal(t, "valid", res.Receivers[0].Name)
	require.Equal(t, "another", res.Receivers[1].Name)
}

func TestListReceivers_SkipsNullEntries(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`[null,{"name":"valid"}]`))
	}))
	defer server.Close()

	client, err := NewClient(Config{AlertmanagerURL: server.URL})
	require.NoError(t, err)

	handler := listReceiversHandler(client)
	_, res, err := handler(context.Background(), nil, ListReceiversReq{})
	require.NoError(t, err)
	require.Len(t, res.Receivers, 1)
	require.Equal(t, "valid", res.Receivers[0].Name)
}

func TestListReceivers_Empty(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode([]*models.Receiver{})
	}))
	defer server.Close()

	client, err := NewClient(Config{AlertmanagerURL: server.URL})
	require.NoError(t, err)

	handler := listReceiversHandler(client)
	_, res, err := handler(context.Background(), nil, ListReceiversReq{})
	require.NoError(t, err)
	require.Empty(t, res.Receivers)
}
