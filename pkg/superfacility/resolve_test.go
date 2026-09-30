package superfacility

import (
	"context"
	"net/http"
	"testing"
)

func TestResolveJobIDAcrossSubmissionStates(t *testing.T) {
	ref := makeTaskJobRef("perlmutter", "task1")
	for _, tc := range []struct {
		name, body, want string
		wantErr          bool
	}{
		{"pending", `{"status":"new"}`, ref, false},
		{"completed", `{"status":"completed","result":"Submitted batch job 12345"}`, "12345", false},
		{"invalid result", `{"status":"completed","result":"error 500"}`, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			client := newTestClient(func(r *http.Request) (*http.Response, error) { calls++; return response(200, tc.body), nil })
			got, err := client.ResolveJobID(context.Background(), ref)
			if (err != nil) != tc.wantErr || got != tc.want {
				t.Fatalf("resolved=%q error=%v", got, err)
			}
			if calls != 1 {
				t.Fatalf("task requests=%d", calls)
			}
			got, err = client.ResolveJobID(context.Background(), "12345")
			if err != nil || got != "12345" || calls != 1 {
				t.Fatal("resolved ID must avoid task lookup")
			}
		})
	}
}
