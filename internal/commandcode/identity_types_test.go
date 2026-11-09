package commandcode

import "testing"

// Validate the shared boundary before independent header/body builders consume
// it: an identity must be a v4-shaped UUID and a self-consistent ASCII workspace.
func TestUpstreamIdentityValidation(t *testing.T) {
	good := UpstreamIdentity{
		ThreadID:    "6ae139ca-c1d9-4793-b78b-17a3888bf11c",
		WorkingDir:  "/home/alex/proj/alpha-services",
		ProjectSlug: "home-alex-proj-alpha-services",
	}
	if err := good.validate(); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*UpstreamIdentity){
		func(i *UpstreamIdentity) { i.ThreadID = "bad" },
		func(i *UpstreamIdentity) { i.ThreadID = "6ae139ca-c1d9-5793-b78b-17a3888bf11c" },
		func(i *UpstreamIdentity) { i.ProjectSlug = "alpha-services" },
		func(i *UpstreamIdentity) { i.WorkingDir = "/home//alex/proj/alpha-services" },
		func(i *UpstreamIdentity) { i.WorkingDir = "/home/alex/../alpha-services" },
	} {
		bad := good
		change(&bad)
		if err := bad.validate(); err == nil {
			t.Errorf("accepted inconsistent identity: %+v", bad)
		}
	}
}
