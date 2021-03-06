package base

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/ghodss/yaml"
	"github.com/qri-io/cafs"
	"github.com/qri-io/dataset"
	"github.com/qri-io/dataset/dsfs"
	"github.com/qri-io/dataset/dsio"
	"github.com/qri-io/ioes"
	"github.com/qri-io/qri/repo"
	"github.com/qri-io/qri/repo/profile"
)

// ListDatasets lists datasets from a repo
func ListDatasets(r repo.Repo, limit, offset int, RPC, publishedOnly bool) (res []repo.DatasetRef, err error) {
	store := r.Store()
	res, err = r.References(limit, offset)
	if err != nil {
		log.Debug(err.Error())
		return nil, fmt.Errorf("error getting dataset list: %s", err.Error())
	}

	if publishedOnly {
		pub := make([]repo.DatasetRef, len(res))
		i := 0
		for _, ref := range res {
			if ref.Published {
				pub[i] = ref
				i++
			}
		}
		res = pub[:i]
	}

	renames := repo.NewNeedPeernameRenames()
	for i, ref := range res {
		// May need to change peername.
		if err := repo.CanonicalizeProfile(r, &res[i], &renames); err != nil {
			return nil, fmt.Errorf("error canonicalizing dataset peername: %s", err.Error())
		}

		ds, err := dsfs.LoadDataset(store, ref.Path)
		if err != nil {
			return nil, fmt.Errorf("error loading path: %s, err: %s", ref.Path, err.Error())
		}
		res[i].Dataset = ds.Encode()
		if RPC {
			res[i].Dataset.Structure.Schema = nil
		}
	}

	// TODO: If renames.Renames is non-empty, apply it to r
	return
}

// CreateDataset uses dsfs to add a dataset to a repo's store, updating all
// references within the repo if successful. CreateDataset is a lower-level
// component of github.com/qri-io/qri/actions.CreateDataset
func CreateDataset(r repo.Repo, streams ioes.IOStreams, name string, ds, dsPrev *dataset.Dataset, body, bodyPrev cafs.File, dryRun, pin bool) (ref repo.DatasetRef, resBody cafs.File, err error) {
	var (
		pro  *profile.Profile
		path string
	)

	pro, err = r.Profile()
	if err != nil {
		return
	}

	// TODO - we should remove the need for this by having viz always be kept in the right
	// state until this point
	if err = prepareViz(r, ds); err != nil {
		return
	}

	// TODO - move dsfs.prepareDataset stuff up here into a "SetComputedValues" func

	if err = ValidateDataset(name, ds); err != nil {
		return
	}

	if path, err = dsfs.CreateDataset(r.Store(), ds, dsPrev, body, bodyPrev, r.PrivateKey(), pin); err != nil {
		return
	}
	if ds.PreviousPath != "" && ds.PreviousPath != "/" {
		prev := repo.DatasetRef{
			ProfileID: pro.ID,
			Peername:  pro.Peername,
			Name:      name,
			Path:      ds.PreviousPath,
		}

		// should be ok to skip this error. we may not have the previous
		// reference locally
		_ = r.DeleteRef(prev)
	}
	ref = repo.DatasetRef{
		ProfileID: pro.ID,
		Peername:  pro.Peername,
		Name:      name,
		Path:      path,
	}
	if err = r.PutRef(ref); err != nil {
		return
	}
	if err = r.LogEvent(repo.ETDsCreated, ref); err != nil {
		return
	}
	_, storeIsPinner := r.Store().(cafs.Pinner)
	if pin && storeIsPinner {
		r.LogEvent(repo.ETDsPinned, ref)
	}

	if err = ReadDataset(r, &ref); err != nil {
		return
	}
	if resBody, err = r.Store().Get(ref.Dataset.BodyPath); err != nil {
		fmt.Println("error getting from store:", err.Error())
	}
	return
}

// FetchDataset grabs a dataset from a remote source
func FetchDataset(r repo.Repo, ref *repo.DatasetRef, pin, load bool) (err error) {
	key := strings.TrimSuffix(ref.Path, "/"+dsfs.PackageFileDataset.String())
	// TODO (b5): use a function from a canonical place to produce this path, possibly from dsfs
	path := key + "/" + dsfs.PackageFileDataset.String()

	fetcher, ok := r.Store().(cafs.Fetcher)
	if !ok {
		err = fmt.Errorf("this store cannot fetch from remote sources")
		return
	}

	// TODO: This is asserting that the target is Fetch-able, but inside dsfs.LoadDataset,
	// only Get is called. Clean up the semantics of Fetch and Get to get this expection
	// more correctly in line with what's actually required.
	_, err = fetcher.Fetch(cafs.SourceAny, key)
	if err != nil {
		return fmt.Errorf("error fetching file: %s", err.Error())
	}

	if pin {
		if err = PinDataset(r, *ref); err != nil {
			log.Debug(err.Error())
			return fmt.Errorf("error pinning root key: %s", err.Error())
		}
	}

	if load {
		ds, err := dsfs.LoadDataset(r.Store(), path)
		if err != nil {
			log.Debug(err.Error())
			return fmt.Errorf("error loading newly saved dataset path: %s", path)
		}

		ref.Dataset = ds.Encode()
	}

	return
}

// ReadDataset grabs a dataset from the store
func ReadDataset(r repo.Repo, ref *repo.DatasetRef) (err error) {
	if store := r.Store(); store != nil {
		ds, e := dsfs.LoadDataset(store, ref.Path)
		if e != nil {
			return e
		}
		ref.Dataset = ds.Encode()
		return
	}

	return cafs.ErrNotFound
}

// PinDataset marks a dataset for retention in a store
func PinDataset(r repo.Repo, ref repo.DatasetRef) error {
	if pinner, ok := r.Store().(cafs.Pinner); ok {
		pinner.Pin(ref.Path, true)
		return r.LogEvent(repo.ETDsPinned, ref)
	}
	return repo.ErrNotPinner
}

// UnpinDataset unmarks a dataset for retention in a store
func UnpinDataset(r repo.Repo, ref repo.DatasetRef) error {
	if pinner, ok := r.Store().(cafs.Pinner); ok {
		pinner.Unpin(ref.Path, true)
		return r.LogEvent(repo.ETDsUnpinned, ref)
	}
	return repo.ErrNotPinner
}

// DatasetPodBodyFile creates a streaming data file from a DatasetPod using the following precedence:
// * dsp.BodyBytes not being nil (requires dsp.Structure.Format be set to know data format)
// * dsp.BodyPath being a url
// * dsp.BodyPath being a path on the local filesystem
// TODO - consider moving this func to some other package. maybe actions?
func DatasetPodBodyFile(store cafs.Filestore, dsp *dataset.DatasetPod) (cafs.File, error) {
	if dsp.BodyBytes != nil {
		if dsp.Structure == nil || dsp.Structure.Format == "" {
			return nil, fmt.Errorf("specifying bodyBytes requires format be specified in dataset.structure")
		}
		return cafs.NewMemfileBytes(fmt.Sprintf("body.%s", dsp.Structure.Format), dsp.BodyBytes), nil
	}

	// all other methods are based on path, bail if we don't have one
	if dsp.BodyPath == "" {
		return nil, nil
	}

	loweredPath := strings.ToLower(dsp.BodyPath)

	// if opening protocol is http/s, we're dealing with a web request
	if strings.HasPrefix(loweredPath, "http://") || strings.HasPrefix(loweredPath, "https://") {
		// TODO - attempt to determine file format based on response headers
		filename := filepath.Base(dsp.BodyPath)

		res, err := http.Get(dsp.BodyPath)
		if err != nil {
			return nil, fmt.Errorf("fetching body url: %s", err.Error())
		}
		if res.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("invalid status code fetching body url: %d", res.StatusCode)
		}

		return cafs.NewMemfileReader(filename, res.Body), nil
	}

	if strings.HasPrefix(dsp.BodyPath, "/ipfs") || strings.HasPrefix(dsp.BodyPath, "/cafs") || strings.HasPrefix(dsp.BodyPath, "/map") {
		return store.Get(dsp.BodyPath)
	}

	// convert yaml input to json as a hack to support yaml input for now
	ext := strings.ToLower(filepath.Ext(dsp.BodyPath))
	if ext == ".yaml" || ext == ".yml" {
		yamlBody, err := ioutil.ReadFile(dsp.BodyPath)
		if err != nil {
			return nil, fmt.Errorf("body file: %s", err.Error())
		}
		jsonBody, err := yaml.YAMLToJSON(yamlBody)
		if err != nil {
			return nil, fmt.Errorf("converting yaml body to json: %s", err.Error())
		}

		filename := fmt.Sprintf("%s.json", strings.TrimSuffix(filepath.Base(dsp.BodyPath), ext))
		return cafs.NewMemfileBytes(filename, jsonBody), nil
	}

	file, err := os.Open(dsp.BodyPath)
	if err != nil {
		return nil, fmt.Errorf("body file: %s", err.Error())
	}

	return cafs.NewMemfileReader(filepath.Base(dsp.BodyPath), file), nil
}

// ConvertBodyFormat rewrites a body from a source format to a destination format.
func ConvertBodyFormat(bodyFile cafs.File, fromSt, toSt *dataset.Structure) (cafs.File, error) {
	// Reader for entries of the source body.
	r, err := dsio.NewEntryReader(fromSt, bodyFile)
	if err != nil {
		return nil, err
	}

	// Writes entries to a new body.
	buffer := &bytes.Buffer{}
	w, err := dsio.NewEntryWriter(toSt, buffer)
	if err != nil {
		return nil, err
	}

	err = dsio.Copy(r, w)
	if err != nil {
		return nil, err
	}
	err = w.Close()
	if err != nil {
		return nil, err
	}

	return cafs.NewMemfileReader(fmt.Sprintf("body.%s", toSt.Format), buffer), nil
}
