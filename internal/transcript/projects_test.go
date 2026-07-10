package transcript_test

import (
	"reflect"
	"testing"

	"github.com/example/agentchat/internal/transcript"
)

func TestProjects(t *testing.T) {
	convs := []*transcript.Conversation{
		{ID: "1", ProjectPath: "/home/u/beta"},
		{ID: "2", ProjectPath: ""}, // scratch: excluded
		{ID: "3", ProjectPath: "/home/u/alpha"},
		{ID: "4", ProjectPath: "/home/u/beta"}, // dedupe + count
		{ID: "5", ProjectPath: "/other/alpha"}, // same label, different path
		{ID: "6", ProjectPath: "/home/u/trail/"},
	}
	got := transcript.Projects(convs)
	want := []transcript.Project{
		{Path: "/home/u/alpha", Label: "alpha", Count: 1},
		{Path: "/other/alpha", Label: "alpha", Count: 1},
		{Path: "/home/u/beta", Label: "beta", Count: 2},
		{Path: "/home/u/trail/", Label: "trail", Count: 1},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Projects:\n got %+v\nwant %+v", got, want)
	}

	if got := transcript.Projects(nil); len(got) != 0 {
		t.Errorf("Projects(nil) = %+v, want empty", got)
	}
	if got := transcript.Projects([]*transcript.Conversation{{ID: "1"}}); len(got) != 0 {
		t.Errorf("scratch-only Projects = %+v, want empty", got)
	}
}
