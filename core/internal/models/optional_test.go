package models

import (
	"encoding/json"
	"testing"
)

// Decoding a partial update document must distinguish omitted fields from
// explicitly-null and explicitly-set ones — that difference is what keeps a
// partial update from wiping fields it did not mention.
func TestUpdateProxyRequest_PartialDecode(t *testing.T) {
	cases := []struct {
		name string
		doc  string
		want UpdateProxyRequest
	}{
		{
			name: "omitted fields stay absent",
			doc:  `{}`,
			want: UpdateProxyRequest{},
		},
		{
			name: "explicit null is present",
			doc:  `{"username":null,"tags":null}`,
			want: UpdateProxyRequest{
				Username: Optional[string]{Present: true, Null: true},
				Tags:     Optional[[]string]{Present: true, Null: true},
			},
		},
		{
			name: "explicit values decode",
			doc:  `{"username":"bob","tags":["a","b"]}`,
			want: UpdateProxyRequest{
				Username: Optional[string]{Present: true, Value: "bob"},
				Tags:     Optional[[]string]{Present: true, Value: []string{"a", "b"}},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got UpdateProxyRequest
			if err := json.Unmarshal([]byte(tc.doc), &got); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if got.Username != tc.want.Username {
				t.Errorf("Username: got %+v, want %+v", got.Username, tc.want.Username)
			}
			if got.Tags.Present != tc.want.Tags.Present ||
				got.Tags.Null != tc.want.Tags.Null ||
				len(got.Tags.Value) != len(tc.want.Tags.Value) {
				t.Errorf("Tags: got %+v, want %+v", got.Tags, tc.want.Tags)
			}
		})
	}
}
