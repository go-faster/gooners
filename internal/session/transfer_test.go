package session

import (
	"testing"
	"time"
)

func TestTransferStats(t *testing.T) {
	startedAt := time.Unix(100, 0)
	lastStatusAt := startedAt.Add(2 * time.Second)
	now := startedAt.Add(5 * time.Second)

	instantSpeedBPS, averageSpeedBPS, etaSeconds := transferStats(
		now,
		startedAt,
		lastStatusAt,
		200,
		500,
		1000,
		false,
	)

	if instantSpeedBPS != 100 {
		t.Fatalf("instant speed = %v, want 100", instantSpeedBPS)
	}
	if averageSpeedBPS != 100 {
		t.Fatalf("average speed = %v, want 100", averageSpeedBPS)
	}
	if etaSeconds != 5 {
		t.Fatalf("eta seconds = %v, want 5", etaSeconds)
	}
}

func TestTransferStatsDone(t *testing.T) {
	startedAt := time.Unix(100, 0)
	lastStatusAt := startedAt.Add(2 * time.Second)
	now := startedAt.Add(5 * time.Second)

	instantSpeedBPS, averageSpeedBPS, etaSeconds := transferStats(
		now,
		startedAt,
		lastStatusAt,
		500,
		1000,
		1000,
		true,
	)

	if instantSpeedBPS != 0 {
		t.Fatalf("instant speed = %v, want 0", instantSpeedBPS)
	}
	if averageSpeedBPS != 0 {
		t.Fatalf("average speed = %v, want 0", averageSpeedBPS)
	}
	if etaSeconds != 0 {
		t.Fatalf("eta seconds = %v, want 0", etaSeconds)
	}
}

func TestTransferSampleUpdateInterval(t *testing.T) {
	startedAt := time.Unix(100, 0)
	now := startedAt.Add(time.Second)

	job := &UploadJob{
		BytesUploaded: 100,
		StartedAt:     startedAt,
		LastStatusAt:  now.Add(-100 * time.Millisecond),
		LastStatus:    50,
	}

	if job.LastStatusAt.IsZero() || now.Sub(job.LastStatusAt) >= minTransferSampleInterval {
		job.LastStatusAt = now
		job.LastStatus = job.BytesUploaded
	}

	if job.LastStatus != 50 {
		t.Fatalf("last status = %v, want 50", job.LastStatus)
	}
}
