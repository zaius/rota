package models

import (
	"encoding/json"
	"testing"
)

// Decoding a partial update document must distinguish omitted fields from
// explicitly-null and explicitly-set ones — that difference is what keeps a
// {"enabled":true} toggle from wiping the user's pool assignment.
func TestUpdateProxyUserRequest_PartialDecode(t *testing.T) {
	cases := []struct {
		name string
		doc  string
		want UpdateProxyUserRequest
	}{
		{
			name: "omitted fields stay absent",
			doc:  `{"enabled":true}`,
			want: UpdateProxyUserRequest{
				MainPoolID:      Optional[int]{},
				FallbackPoolIDs: Optional[[]int]{},
			},
		},
		{
			name: "explicit null is present",
			doc:  `{"main_pool_id":null,"fallback_pool_ids":null}`,
			want: UpdateProxyUserRequest{
				MainPoolID:      Optional[int]{Present: true, Null: true},
				FallbackPoolIDs: Optional[[]int]{Present: true, Null: true},
			},
		},
		{
			name: "explicit values decode",
			doc:  `{"main_pool_id":3,"fallback_pool_ids":[4,5]}`,
			want: UpdateProxyUserRequest{
				MainPoolID:      Optional[int]{Present: true, Value: 3},
				FallbackPoolIDs: Optional[[]int]{Present: true, Value: []int{4, 5}},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got UpdateProxyUserRequest
			if err := json.Unmarshal([]byte(tc.doc), &got); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if got.MainPoolID.Present != tc.want.MainPoolID.Present ||
				got.MainPoolID.Null != tc.want.MainPoolID.Null ||
				got.MainPoolID.Value != tc.want.MainPoolID.Value {
				t.Errorf("MainPoolID: got %+v, want %+v", got.MainPoolID, tc.want.MainPoolID)
			}
			if got.FallbackPoolIDs.Present != tc.want.FallbackPoolIDs.Present ||
				got.FallbackPoolIDs.Null != tc.want.FallbackPoolIDs.Null ||
				len(got.FallbackPoolIDs.Value) != len(tc.want.FallbackPoolIDs.Value) {
				t.Errorf("FallbackPoolIDs: got %+v, want %+v", got.FallbackPoolIDs, tc.want.FallbackPoolIDs)
			}
		})
	}
}
