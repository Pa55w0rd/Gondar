package selfupdate

import (
	"context"
	"errors"
	"net/http"
	"testing"
)

type rejectReleaseNetwork struct{ t *testing.T }

func (r rejectReleaseNetwork) RoundTrip(*http.Request) (*http.Response, error) {
	r.t.Fatal("未配置发布源的构建不应访问网络")
	return nil, errors.New("unexpected network request")
}

func TestUnconfiguredReleaseSourceDoesNotAccessNetwork(t *testing.T) {
	previous := Repo
	Repo = ""
	t.Cleanup(func() { Repo = previous })
	client := &http.Client{Transport: rejectReleaseNetwork{t}}
	if _, err := FetchLatest(context.Background(), client); !errors.Is(err, ErrReleaseSourceUnset) {
		t.Fatalf("FetchLatest error = %v", err)
	}
	if _, err := FetchByTag(context.Background(), client, "v1.2.0"); !errors.Is(err, ErrReleaseSourceUnset) {
		t.Fatalf("FetchByTag error = %v", err)
	}
}
