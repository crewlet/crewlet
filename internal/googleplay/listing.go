package googleplay

import (
	"context"
	"fmt"
	"slices"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/googlegrowth"
	"github.com/crewlet/crewlet/internal/growth"
	"github.com/crewlet/crewlet/internal/words"
)

const (
	opListingsPatch   = "androidpublisher.edits.listings.patch"
	opListingsList    = "androidpublisher.edits.listings.list"
	opDetailsPatch    = "androidpublisher.edits.details.patch"
	opDetailsGet      = "androidpublisher.edits.details.get"
	opImagesList      = "androidpublisher.edits.images.list"
	opImagesUpload    = "androidpublisher.edits.images.upload"
	opImagesDelete    = "androidpublisher.edits.images.delete"
	opImagesDeleteAll = "androidpublisher.edits.images.deleteall"
	opTracksList      = "androidpublisher.edits.tracks.list"
)

// listingAct is google_play_update_listing's act: one language's listing
// text and the app's details, each within Play's limits, in one edit.
func (p *Product) listingAct(call growth.Call) (act, error) {
	app, _ := call.Args["app"].(string)
	a := act{app: app, edit: true, summary: "change the store listing"}
	listing := map[string]any{}
	for arg, field := range map[string]string{"title": "title", "short_description": "shortDescription",
		"full_description": "fullDescription", "video": "video"} {
		if v, ok := call.Args[arg].(string); ok {
			listing[field] = v
		}
	}
	for field, limit := range map[string]int{"title": titleLimit, "shortDescription": shortLimit,
		"fullDescription": fullLimit} {
		if v, _ := listing[field].(string); utf8.RuneCountInString(v) > limit {
			return act{}, refusal(growth.RefusedInvalid, fmt.Sprintf("%s is at most %d characters on Google "+
				"Play; it is %d. No vendor was reached.", field, limit, utf8.RuneCountInString(v)))
		}
	}
	if len(listing) > 0 {
		language, _ := call.Args["language"].(string)
		op, _ := p.Catalog().Operation(opListingsPatch)
		req, err := newRequest(op, map[string]string{"packageName": app, "language": language}, nil, listing)
		if err != nil {
			return act{}, err
		}
		a.steps = append(a.steps, step{req: req})
	}
	details := map[string]any{}
	for arg, field := range map[string]string{"contact_email": "contactEmail", "contact_phone": "contactPhone",
		"contact_website": "contactWebsite", "default_language": "defaultLanguage"} {
		if v, ok := call.Args[arg].(string); ok {
			details[field] = v
		}
	}
	if len(details) > 0 {
		op, _ := p.Catalog().Operation(opDetailsPatch)
		req, err := newRequest(op, map[string]string{"packageName": app}, nil, details)
		if err != nil {
			return act{}, err
		}
		a.steps = append(a.steps, step{req: req})
	}
	if len(a.steps) == 0 {
		return act{}, refusal(growth.RefusedInvalid, "name a listing field (title, descriptions, video) with its "+
			"language, or a contact detail. No vendor was reached.")
	}
	return a, nil
}

// imagesAct is google_play_update_images's act: images of one language and
// type removed and uploaded from artifacts, in one edit.
func (p *Product) imagesAct(ctx context.Context, call growth.Call) (act, error) {
	app, _ := call.Args["app"].(string)
	language, _ := call.Args["language"].(string)
	kind, _ := call.Args["image_type"].(string)
	if !slices.Contains(imageTypes(), kind) {
		return act{}, refusal(growth.RefusedInvalid, "image_type is one of "+words.Or(imageTypes())+
			". No vendor was reached.")
	}
	path := map[string]string{"packageName": app, "language": language, "imageType": kind}
	a := act{app: app, edit: true, summary: "change the " + kind + " images (" + language + ")"}
	add := func(id string, path map[string]string, media *File) error {
		op, _ := p.Catalog().Operation(id)
		req, err := newRequest(op, path, nil, nil)
		if err != nil {
			return err
		}
		a.steps = append(a.steps, step{req: req, media: media})
		return nil
	}
	if all, _ := call.Args["remove_all"].(bool); all {
		if err := add(opImagesDeleteAll, path, nil); err != nil {
			return act{}, err
		}
	}
	for _, id := range stringList(call.Args["remove"]) {
		if err := add(opImagesDelete, withParam(path, "imageId", id), nil); err != nil {
			return act{}, err
		}
	}
	upload, _ := p.Catalog().Operation(opImagesUpload)
	for _, ref := range stringList(call.Args["upload"]) {
		file, err := p.mediaOf(ctx, call.Handle, upload, ref)
		if err != nil {
			return act{}, err
		}
		if err := add(opImagesUpload, path, file); err != nil {
			return act{}, err
		}
	}
	if len(a.steps) == 0 || len(a.steps) > maxSteps {
		return act{}, refusal(growth.RefusedInvalid, "name 1 to 20 images to upload or remove. No vendor was reached.")
	}
	return a, nil
}

// Listing is the listing tool's answer: Google's listings, details and images.
type Listing struct {
	App      string `json:"app"`
	Details  string `json:"details" untrusted:"fence"`
	Listings string `json:"listings" untrusted:"fence"`
	Images   string `json:"images,omitempty" untrusted:"fence"`
}

// readListing reads an app's listings, details and, where named, images, in
// one throwaway edit.
func (p *Product) readListing(ctx context.Context, call growth.Call) (any, error) {
	app, _ := call.Args["app"].(string)
	language, _ := call.Args["language"].(string)
	kind, _ := call.Args["image_type"].(string)
	reqs := map[string]googlegrowth.Request{}
	for _, id := range []string{opDetailsGet, opListingsList} {
		op, _ := p.Catalog().Operation(id)
		req, err := newRequest(op, map[string]string{"packageName": app}, nil, nil)
		if err != nil {
			return nil, err
		}
		reqs[id] = req
	}
	if kind != "" {
		op, _ := p.Catalog().Operation(opImagesList)
		req, err := newRequest(op, map[string]string{"packageName": app, "language": language, "imageType": kind},
			nil, nil)
		if err != nil {
			return nil, err
		}
		reqs[opImagesList] = req
	}
	if err := p.inScope(call.Handle, reqs[opDetailsGet]); err != nil {
		return nil, err
	}
	answers := map[string]string{}
	err := p.inThrowawayEdit(ctx, call.Handle, app, func(id string) error {
		for name, req := range reqs {
			out, err := p.client.SendAndRead(ctx, call.Handle, inEdit(req, id), nil)
			if err != nil {
				return err
			}
			answers[name] = out.Answer
		}
		return nil
	})
	p.client.MadeOrFailed(ctx, call, reqs[opListingsList], growth.ClassRead, err)
	if err != nil {
		return nil, err
	}
	return Listing{App: app, Details: answers[opDetailsGet], Listings: answers[opListingsList],
		Images: answers[opImagesList]}, nil
}

// Tracks is the tracks tool's answer.
type Tracks struct {
	App    string `json:"app"`
	Tracks string `json:"tracks" untrusted:"fence"`
}

// readTracks reads an app's tracks in a throwaway edit.
func (p *Product) readTracks(ctx context.Context, call growth.Call) (any, error) {
	app, _ := call.Args["app"].(string)
	op, _ := p.Catalog().Operation(opTracksList)
	req, err := newRequest(op, map[string]string{"packageName": app}, nil, nil)
	if err != nil {
		return nil, err
	}
	if err = p.inScope(call.Handle, req); err != nil {
		return nil, err
	}
	answer, err := p.readStaged(ctx, call, req)
	if err != nil {
		return nil, err
	}
	return Tracks{App: app, Tracks: answer.(googlegrowth.Answer).Answer}, nil
}
