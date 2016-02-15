package app

import (
	"net/http"

	"github.com/sourcegraph/mux"

	"src.sourcegraph.com/sourcegraph/app/internal/schemautil"
	"src.sourcegraph.com/sourcegraph/app/internal/tmpl"
	"src.sourcegraph.com/sourcegraph/go-sourcegraph/sourcegraph"
	"src.sourcegraph.com/sourcegraph/ui/payloads"
	"src.sourcegraph.com/sourcegraph/util/handlerutil"
)

// serveDef creates a new response for the code view that contains information
// about a definition. Additionally, it may also contain information about the
// tree entry that contains that definition.
func serveDef(w http.ResponseWriter, r *http.Request) error {
	ctx, cl := handlerutil.Client(r)

	dc, rc, vc, err := handlerutil.GetDefCommon(ctx, mux.Vars(r), &sourcegraph.DefGetOptions{Doc: true})
	if err != nil {
		return err
	}

	if isVirtual(dc.Def.DefKey) {
		return serveDefVirtual(w, r, dc, rc, vc)
	}

	tc := &handlerutil.TreeEntryCommon{
		EntrySpec: sourcegraph.TreeEntrySpec{
			RepoRev: vc.RepoRevSpec,
			Path:    dc.Def.File,
		},
	}
	tc.Entry, err = cl.RepoTree.Get(ctx, &sourcegraph.RepoTreeGetOp{Entry: tc.EntrySpec, Opt: &sourcegraph.RepoTreeGetOptions{}})
	if err != nil {
		return err
	}

	return serveRepoTreeEntry(w, r, tc, rc, vc, dc)
}

func serveDefExamples(w http.ResponseWriter, r *http.Request) error {
	var opt sourcegraph.DefListExamplesOptions
	err := schemautil.Decode(&opt, r.URL.Query())
	if err != nil {
		return err
	}

	ctx, cl := handlerutil.Client(r)

	dc, rc, vc, err := handlerutil.GetDefCommon(ctx, mux.Vars(r), nil)
	if err != nil {
		return err
	}

	// Get actual list of examples
	examples, err := cl.Defs.ListExamples(ctx, &sourcegraph.DefsListExamplesOp{
		Def: dc.Def.DefSpec(),
		Rev: vc.RepoRevSpec.Rev,
		Opt: &opt,
	})
	if err != nil {
		return err
	}

	pg, err := paginatePrevNext(opt, examples.StreamResponse)
	if err != nil {
		return err
	}

	return tmpl.Exec(r, w, "def/examples.html", http.StatusOK, nil, &struct {
		handlerutil.RepoCommon
		handlerutil.RepoRevCommon
		payloads.DefCommon
		Examples  []*sourcegraph.Example
		PageLinks []pageLink
		Options   sourcegraph.DefListExamplesOptions
		tmpl.Common
	}{
		RepoCommon:    *rc,
		RepoRevCommon: *vc,
		DefCommon:     *dc,
		Examples:      examples.Examples,
		PageLinks:     pg,
		Options:       opt,
	})
}

func serveDefPopover(w http.ResponseWriter, r *http.Request) error {
	// viewState holds the repository and file that the user is currently
	// viewing.
	type viewState struct {
		CurrentRepo string
		CurrentFile string
	}
	var opt viewState
	err := schemautil.Decode(&opt, r.URL.Query())
	if err != nil {
		return err
	}
	ctx, _ := handlerutil.Client(r)
	dc, _, _, err := handlerutil.GetDefCommon(ctx, mux.Vars(r), &sourcegraph.DefGetOptions{Doc: true})
	if err != nil {
		// TODO(gbbr): Set up custom responses for each scenario.
		// All of the below errors will cause full page HTML pages or redirects, if
		// bubbled up the chain, so we return nil instead.
		// Temporarily StatusNotFound with empty body will be returned.
		switch err.(type) {
		case *handlerutil.URLMovedError, *handlerutil.NoVCSDataError:
			http.Error(w, "", http.StatusNotFound)
			return nil
		}
		return err
	}

	hdr := http.Header{
		"access-control-allow-origin":  []string{"*"},
		"access-control-allow-methods": []string{"GET"},
	}

	return tmpl.Exec(r, w, "def/popover.html", http.StatusOK, hdr, &struct {
		Def         *sourcegraph.Def
		CurrentRepo string
		CurrentFile string
		tmpl.Common
	}{
		Def:         dc.Def,
		CurrentRepo: opt.CurrentRepo,
		CurrentFile: opt.CurrentFile,
	})
}

// defRobotsIndex returns a boolean indicating whether the page
// corresponding to this def should be indexed by robots (e.g.,
// Googlebot).
func defRobotsIndex(repo *sourcegraph.Repo, def *sourcegraph.Def) bool {
	return !repo.Private && def.Exported && !def.Test
}
