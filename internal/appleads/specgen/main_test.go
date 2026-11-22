package main

import (
	"strings"
	"testing"
)

const sample = `
  @GET("campaigns/{id}")
  Call<CampaignResponse> campaignsIdGet(
    @retrofit2.http.Header("X-Ap-Context") String xApContext, @retrofit2.http.Path("id") Long id
  );

  @retrofit2.http.Multipart
  @POST("assets/upload")
  Call<AssetResponse> uploadAsset(
    @retrofit2.http.Part MultipartBody.Part _file
  );
`

// The check reads each Retrofit method's verb, path and name, and reports an
// operation the table lacks, one it has that the client does not, and one
// whose ID differs.
//
// Mutation: skip the gone or renamed cases, and a reclassed or retired
// operation passes the check.
func TestTheCheckFindsEveryDisagreement(t *testing.T) {
	t.Parallel()
	ops, err := readClient(sample)
	if err != nil || len(ops) != 2 || ops[0] != (operation{"campaignsIdGet", "GET", "/v1/campaigns/{id}"}) ||
		ops[1] != (operation{"uploadAsset", "POST", "/v1/assets/upload"}) {
		t.Fatalf("read %+v (%v)", ops, err)
	}
	got := strings.Join(diff(ops, map[string]string{"GET /v1/campaigns/{id}": "getCampaign",
		"DELETE /v1/campaigns/{id}": "campaignsIdDelete"}), "\n")
	for _, want := range []string{"added: POST /v1/assets/upload (uploadAsset)",
		"renamed: GET /v1/campaigns/{id} is campaignsIdGet", "gone: DELETE /v1/campaigns/{id}"} {
		if !strings.Contains(got, want) {
			t.Errorf("the check said\n%s\nwant %q", got, want)
		}
	}
	if d := diff(ops, map[string]string{"GET /v1/campaigns/{id}": "campaignsIdGet",
		"POST /v1/assets/upload": "uploadAsset"}); len(d) != 0 {
		t.Errorf("an agreeing table answered %v", d)
	}
}
