package agent

import (
	"errors"

	"gondar/db"
	"gondar/filepreview"
)

// Optional host capability: only the Worker instance supplies this intent ID.
// save_evidence has no model-controlled provenance parameter or path inference.
type IntentArtifactRecorder interface {
	RecordArtifactForIntent(taskID, intentID, findingID int64, runDir, relPath, name, kind, sourceURL, note string) (*db.TaskArtifact, error)
}

type IntentArtifactProgressRecorder interface {
	RecordArtifactForIntentWithProgress(taskID, intentID, findingID int64, runDir, relPath, name, kind, sourceURL, note string) (*db.TaskArtifact, bool, error)
}

type intentArtifactRecorder struct {
	inner            ArtifactRecorder
	taskID, intentID int64
}

func artifactRecorderForIntent(inner ArtifactRecorder, taskID, intentID int64) ArtifactRecorder {
	if inner == nil {
		return nil
	}
	recorder := intentArtifactRecorder{inner: inner, taskID: taskID, intentID: intentID}
	if previewer, ok := inner.(ArtifactPreviewer); ok {
		return intentArtifactPreviewRecorder{intentArtifactRecorder: recorder, previewer: previewer}
	}
	return recorder
}

// Optional capabilities survive the provenance wrapper only when the underlying
// host really provides them. The host retains artifact ownership/path/size checks.
type intentArtifactPreviewRecorder struct {
	intentArtifactRecorder
	previewer ArtifactPreviewer
}

func (r intentArtifactPreviewRecorder) PreviewArtifactContent(taskID, artifactID int64, entry string) (*filepreview.Preview, error) {
	if taskID != r.taskID || taskID <= 0 || r.intentID <= 0 {
		return nil, errors.New("证据预览缺少当前任务的真实执行意图")
	}
	return r.previewer.PreviewArtifactContent(taskID, artifactID, entry)
}

func (r intentArtifactRecorder) RecordArtifact(taskID, findingID int64, runDir, relPath, name, kind, sourceURL, note string) (*db.TaskArtifact, error) {
	if taskID != r.taskID || taskID <= 0 || r.intentID <= 0 {
		return nil, errors.New("证据登记缺少当前任务的真实执行意图")
	}
	if host, ok := r.inner.(IntentArtifactRecorder); ok {
		return host.RecordArtifactForIntent(taskID, r.intentID, findingID, runDir, relPath, name, kind, sourceURL, note)
	}
	return r.inner.RecordArtifact(taskID, findingID, runDir, relPath, name, kind, sourceURL, note)
}

func (r intentArtifactRecorder) RecordArtifactWithProgress(taskID, findingID int64, runDir, relPath, name, kind, sourceURL, note string) (*db.TaskArtifact, bool, error) {
	if taskID != r.taskID || taskID <= 0 || r.intentID <= 0 {
		return nil, false, errors.New("证据登记缺少当前任务的真实执行意图")
	}
	if host, ok := r.inner.(IntentArtifactProgressRecorder); ok {
		return host.RecordArtifactForIntentWithProgress(taskID, r.intentID, findingID, runDir, relPath, name, kind, sourceURL, note)
	}
	if _, ok := r.inner.(IntentArtifactRecorder); !ok {
		if host, ok := r.inner.(ArtifactProgressRecorder); ok {
			return host.RecordArtifactWithProgress(taskID, findingID, runDir, relPath, name, kind, sourceURL, note)
		}
	}
	rec, err := r.RecordArtifact(taskID, findingID, runDir, relPath, name, kind, sourceURL, note)
	return rec, false, err
}
