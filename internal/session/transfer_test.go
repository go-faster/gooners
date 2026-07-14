package session

import (
	"testing"
	"time"
)

func TestTransferStats(t *testing.T) {
	startedAt := time.Unix(100, 0)
	lastStatusAt := startedAt.Add(2 * time.Second)
	now := startedAt.Add(5 * time.Second)

	instantSpeedBPS, averageSpeedBPS, durationSeconds, etaSeconds := transferStats(
		now,
		startedAt,
		time.Time{},
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
	if durationSeconds != 5 {
		t.Fatalf("duration seconds = %v, want 5", durationSeconds)
	}
	if etaSeconds != 5 {
		t.Fatalf("eta seconds = %v, want 5", etaSeconds)
	}
}

func TestTransferStatsDone(t *testing.T) {
	startedAt := time.Unix(100, 0)
	lastStatusAt := startedAt.Add(2 * time.Second)
	finishedAt := startedAt.Add(5 * time.Second)
	now := startedAt.Add(time.Minute)

	instantSpeedBPS, averageSpeedBPS, durationSeconds, etaSeconds := transferStats(
		now,
		startedAt,
		finishedAt,
		lastStatusAt,
		700,
		1000,
		1000,
		true,
	)

	if instantSpeedBPS != 100 {
		t.Fatalf("instant speed = %v, want 100", instantSpeedBPS)
	}
	if averageSpeedBPS != 200 {
		t.Fatalf("average speed = %v, want 200", averageSpeedBPS)
	}
	if durationSeconds != 5 {
		t.Fatalf("duration seconds = %v, want 5", durationSeconds)
	}
	if etaSeconds != 0 {
		t.Fatalf("eta seconds = %v, want 0", etaSeconds)
	}
}

func TestTransferStatsDoneWithoutLastSample(t *testing.T) {
	startedAt := time.Unix(100, 0)
	finishedAt := startedAt.Add(5 * time.Second)
	now := startedAt.Add(time.Minute)

	instantSpeedBPS, averageSpeedBPS, durationSeconds, etaSeconds := transferStats(
		now,
		startedAt,
		finishedAt,
		time.Time{},
		0,
		1000,
		1000,
		true,
	)

	if instantSpeedBPS != 200 {
		t.Fatalf("instant speed = %v, want 200", instantSpeedBPS)
	}
	if averageSpeedBPS != 200 {
		t.Fatalf("average speed = %v, want 200", averageSpeedBPS)
	}
	if durationSeconds != 5 {
		t.Fatalf("duration seconds = %v, want 5", durationSeconds)
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
