package artifactdataapi

import (
	"github.com/crewlet/crewlet/internal/api/artifactsapi"
	"github.com/crewlet/crewlet/internal/artifactdata"
	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE ADMISSION IS THE ARTIFACTS' OWN ([artifactsapi.Gate]): the datastore's
// routes and its read questions decide who may touch an artifact's data by the
// very gate the artifacts' routes and questions decide who may touch the
// artifact by, over the same head, chart and company settings. A second gate
// here would be the place the two first disagreed about who holds a role on
// one artifact. What is the datastore's own is how an admission shapes a read
// and stamps a write.

// Reading is the admission as a datastore read is shaped by it: the reader's
// record, and their exact role on the artifact, which each collection's read
// rule compares.
func Reading(a artifactsapi.Admission, fresh statelog.Freshness) artifactdata.Reading {
	return artifactdata.Reading{Record: iam.RecordOwner(a.Principal), Role: a.Role(), Fresh: fresh}
}

// authority is the admission as a datastore write is stamped with it: the
// actor, the exact role (a datastore verb's admission resolves it through the
// read row, and an unsettled one is refused at the gate), whether only the
// admin path admitted the caller, the plan quotas and the surface the write
// came through.
func authority(a artifactsapi.Admission, origin artifacts.WriteOrigin, limits artifacts.Limits) artifacts.Authority {
	return artifacts.Authority{
		Actor: iam.ActorFor(a.Principal), Role: a.Role(), Elevated: a.Elevated(),
		Limits: limits, Origin: origin,
	}
}
