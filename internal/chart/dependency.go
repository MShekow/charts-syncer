package chart

import (
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/juju/errors"
	"github.com/mkmik/multierror"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/provenance"
	"io/ioutil"
	"k8s.io/klog"
	"net/url"
	"os"
	"path"
	"sigs.k8s.io/yaml"

	"github.com/bitnami-labs/charts-syncer/api"
	"github.com/bitnami-labs/charts-syncer/internal/utils"
	"github.com/bitnami-labs/charts-syncer/pkg/client"
)

// dependencies is the list of dependencies of a chart
type dependencies struct {
	Dependencies []*chart.Dependency `json:"dependencies"`
}

// lockFilePath returns the path to the lock file according to provided Api version
func lockFilePath(chartPath, apiVersion string) (string, error) {
	switch apiVersion {
	case APIV1:
		return path.Join(chartPath, RequirementsLockFilename), nil
	case APIV2:
		return path.Join(chartPath, ChartLockFilename), nil
	default:
		return "", errors.Errorf("unrecognised apiVersion %q", apiVersion)
	}
}

// GetChartLock returns the chart.Lock from an uncompressed chart
func GetChartLock(chartPath string) (*chart.Lock, error) {
	// If the API version is not set, there is not a lock file. Hence, this
	// chart has no dependencies.
	apiVersion, err := GetLockAPIVersion(chartPath)
	if err != nil {
		return nil, errors.Trace(err)
	}
	if apiVersion == "" {
		return nil, nil
	}

	lockFilePath, err := lockFilePath(chartPath, apiVersion)
	if err != nil {
		return nil, errors.Trace(err)
	}
	lockContent, err := ioutil.ReadFile(lockFilePath)
	if err != nil {
		return nil, errors.Trace(err)
	}
	lock := &chart.Lock{}
	if err = yaml.Unmarshal(lockContent, lock); err != nil {
		return nil, errors.Annotatef(err, "unmarshaling %q file", lockFilePath)
	}
	return lock, nil
}

// GetChartDependencies returns the chart dependencies from a chart in tgz format.
func GetChartDependencies(filepath string, name string) ([]*chart.Dependency, error) {
	// Create temporary working directory
	chartPath, err := ioutil.TempDir("", "charts-syncer")
	if err != nil {
		return nil, errors.Trace(err)
	}
	defer os.RemoveAll(chartPath)

	// Uncompress chart
	if err := utils.Untar(filepath, chartPath); err != nil {
		return nil, errors.Annotatef(err, "uncompressing %q", filepath)
	}
	// Untar uncompress the chart in a subfolder
	chartPath = path.Join(chartPath, name)

	lock, err := GetChartLock(chartPath)
	if err != nil {
		return nil, errors.Trace(err)
	}
	if lock != nil {
		return lock.Dependencies, nil
	}

	// Try fallback from the dependencies
	metadata, err := chartutil.LoadChartfile(path.Join(chartPath, ChartFilename))
	if err != nil {
		return nil, errors.Trace(err)
	}
	if metadata.Dependencies != nil {
		return metadata.Dependencies, nil
	}
	return nil, nil
}

// GetLockAPIVersion returns the apiVersion field of a chart's lock file
func GetLockAPIVersion(chartPath string) (string, error) {
	if ok, err := utils.FileExists(path.Join(chartPath, RequirementsLockFilename)); err != nil {
		return "", errors.Trace(err)
	} else if ok {
		return APIV1, nil
	}
	if ok, err := utils.FileExists(path.Join(chartPath, ChartLockFilename)); err != nil {
		return "", errors.Trace(err)
	} else if ok {
		return APIV2, nil
	}

	return "", nil
}

// BuildDependencies updates the chart dependencies and their repository references in the provided chart path
//
// It reads the lock file (or, if unavailable, the Chart.yaml file) to download the versions from the target chart repository
func BuildDependencies(chartPath string, r client.ChartsReader, sourceRepo, targetRepo *api.Repo, t map[uint32]client.ChartsReaderWriter, syncTrusted, ignoreTrusted []*api.Repo) error {

	// Build deps manually for OCI as helm does not support it yet
	if err := os.RemoveAll(path.Join(chartPath, "charts")); err != nil {
		return errors.Trace(err)
	}
	// Re-create empty charts folder
	err := os.Mkdir(path.Join(chartPath, "charts"), 0755)
	if err != nil {
		return errors.Trace(err)
	}
	lock, err := GetChartLock(chartPath)
	if err != nil {
		return errors.Trace(err)
	}
	// Step 1. Update references in the dependencies object
	// If the API version is not set, there is not a lock file. Hence, this
	// chart has no dependencies.
	apiVersion, err := GetLockAPIVersion(chartPath)
	if err != nil {
		return errors.Trace(err)
	}

	var depsFromMetadata []*chart.Dependency
	if apiVersion == "" {
		// Neither a Chart.lock nor requirements.lock file exist, but if the Chart.yaml has V2 API version, the
		// dependencies are still declared in the Chart.yaml itself
		metadata, err := chartutil.LoadChartfile(path.Join(chartPath, ChartFilename))
		if err != nil {
			return errors.Trace(err)
		}
		if metadata.APIVersion == chart.APIVersionV2 {
			apiVersion = APIV2
			depsFromMetadata = metadata.Dependencies
		} else {
			return nil
		}

	}

	switch apiVersion {
	case APIV1:
		if err := updateRequirementsFile(chartPath, lock, sourceRepo, targetRepo, syncTrusted, ignoreTrusted); err != nil {
			return errors.Trace(err)
		}
	case APIV2:
		if err := updateChartMetadataFile(chartPath, lock, sourceRepo, targetRepo, syncTrusted, ignoreTrusted); err != nil {
			return errors.Trace(err)
		}
	default:
		return errors.Errorf("unrecognised apiVersion %s", apiVersion)
	}

	// Step 2. Build charts/ folder
	var deps []*chart.Dependency
	if lock != nil {
		deps = lock.Dependencies
	} else if depsFromMetadata != nil {
		deps = depsFromMetadata
	}
	var errs error

	if deps != nil {
		for _, dep := range deps {
			id := fmt.Sprintf("%s-%s", dep.Name, dep.Version)
			klog.V(4).Infof("Building %q chart dependency", id)

			var repoClient client.ChartsReader = nil

			depRepo := api.Repo{
				Url: dep.Repository,
			}

			//if the repo is trusted and won't be synced - we download the dependency from it (source)
			if utils.ShouldIgnoreRepo(depRepo, syncTrusted, ignoreTrusted) {
				repoClient = t[utils.GetRepoLocationId(dep.Repository)]
			} else {
				//otherwise we download it from the destination repo
				repoClient = r
			}

			depTgz, err := repoClient.Fetch(dep.Name, dep.Version)

			if err != nil {
				klog.Warningf("Failed fetching %q chart. The dependencies processing will remain incomplete.", id)
				errs = multierror.Append(errs, errors.Annotatef(err, "fetching %q chart", id))
				continue
			}

			depFile := path.Join(chartPath, "charts", fmt.Sprintf("%s.tgz", id))
			if err := utils.CopyFile(depFile, depTgz); err != nil {
				klog.Warningf("Failed copying %q chart. The dependencies processing will remain incomplete.", id)
				errs = multierror.Append(errs, errors.Annotatef(err, "copying %q chart to %q", id, depFile))
				continue
			}
		}
	}

	return errs
}

// updateChartMetadataFile updates the dependencies in Chart.yaml
// For helm v3 dependency management
func updateChartMetadataFile(chartPath string, lock *chart.Lock, sourceRepo, targetRepo *api.Repo, syncTrusted, ignoreTrusted []*api.Repo) error {
	chartFile := path.Join(chartPath, ChartFilename)
	chartYamlContent, err := ioutil.ReadFile(chartFile)
	if err != nil {
		return errors.Trace(err)
	}
	chartMetadata := &chart.Metadata{}
	err = yaml.Unmarshal(chartYamlContent, chartMetadata)
	if err != nil {
		return errors.Annotatef(err, "error unmarshaling %s file", chartFile)
	}
	for _, dep := range chartMetadata.Dependencies {
		// Maybe there are dependencies from other chart repos. We replace them or not depending on what we have in
		// source.ignoreTrustedRepos and target.syncTrustedRepos (the logic can be found in utils.ShouldIgnoreRepo)
		r := api.Repo{
			Url: dep.Repository,
		}

		//ignore repo means don't replace it, don't ignore - means "replace it" - use negation to achieve it
		replaceDependencyRepo := !utils.ShouldIgnoreRepo(r, syncTrusted, ignoreTrusted)

		if dep.Repository == sourceRepo.GetUrl() || replaceDependencyRepo {
			repoUrl, err := getDependencyRepoURL(targetRepo)
			if err != nil {
				return errors.Trace(err)
			}
			dep.Repository = repoUrl
		}
	}
	// Write updated chart yaml file
	dest := path.Join(chartPath, ChartFilename)
	if err := writeChartFile(dest, chartMetadata); err != nil {
		return errors.Trace(err)
	}
	if lock != nil {
		if err := updateLockFile(chartPath, lock, chartMetadata.Dependencies, sourceRepo, targetRepo, false, syncTrusted, ignoreTrusted); err != nil {
			return errors.Trace(err)
		}
	}
	return nil
}

// updateRequirementsFile returns the full list of dependencies and the list of missing dependencies.
// For helm v2 dependency management
func updateRequirementsFile(chartPath string, lock *chart.Lock, sourceRepo, targetRepo *api.Repo, syncTrusted, ignoreTrusted []*api.Repo) error {
	requirementsFile := path.Join(chartPath, RequirementsFilename)
	requirements, err := ioutil.ReadFile(requirementsFile)
	if err != nil {
		return errors.Trace(err)
	}

	deps := &dependencies{}
	err = yaml.Unmarshal(requirements, deps)
	if err != nil {
		return errors.Annotatef(err, "error unmarshaling %s file", requirementsFile)
	}
	for _, dep := range deps.Dependencies {
		// Maybe there are dependencies from other chart repos. We replace them or not depending on what we have in
		// source.ignoreTrustedRepos and target.syncTrustedRepos (the logic can be found in utils.ShouldIgnoreRepo)
		r := api.Repo{
			Url: dep.Repository,
		}

		//ignore repo means don't replace it, don't ignore - means "replace it" - use negation to achieve it
		replaceDependencyRepo := !utils.ShouldIgnoreRepo(r, syncTrusted, ignoreTrusted)

		// For example, old charts pointing to helm/charts repo
		if dep.Repository == sourceRepo.GetUrl() || replaceDependencyRepo {
			repoUrl, err := getDependencyRepoURL(targetRepo)
			if err != nil {
				return errors.Trace(err)
			}
			dep.Repository = repoUrl
		}
	}
	// Write updated requirements yamls file
	dest := path.Join(chartPath, RequirementsFilename)
	if err := writeChartFile(dest, deps); err != nil {
		return errors.Trace(err)
	}
	if err := updateLockFile(chartPath, lock, deps.Dependencies, sourceRepo, targetRepo, true, syncTrusted, ignoreTrusted); err != nil {
		return errors.Trace(err)
	}
	return nil
}

// updateLockFile updates the lock file with the new registry
func updateLockFile(chartPath string, lock *chart.Lock, deps []*chart.Dependency, sourceRepo *api.Repo, targetRepo *api.Repo, legacyLockfile bool, syncTrusted, ignoreTrusted []*api.Repo) error {
	for _, dep := range lock.Dependencies {

		// Maybe there are dependencies from other chart repos. We replace them or not depending on what we have in
		// source.ignoreTrustedRepos and target.syncTrustedRepos (the logic can be found in utils.ShouldIgnoreRepo)
		r := api.Repo{
			Url: dep.Repository,
		}

		//ignore repo means don't replace it, don't ignore - means "replace it" - use negation to achieve it
		replaceDependencyRepo := !utils.ShouldIgnoreRepo(r, syncTrusted, ignoreTrusted)

		if dep.Repository == sourceRepo.GetUrl() || replaceDependencyRepo {
			repoUrl, err := getDependencyRepoURL(targetRepo)
			if err != nil {
				return errors.Trace(err)
			}
			dep.Repository = repoUrl
		}
	}
	newDigest, err := hashDeps(deps, lock.Dependencies)
	if err != nil {
		return errors.Trace(err)
	}
	lock.Digest = newDigest

	// Write updated lock file
	lockFileName := ChartLockFilename
	if legacyLockfile {
		lockFileName = RequirementsLockFilename
	}
	dest := path.Join(chartPath, lockFileName)
	if err := writeChartFile(dest, lock); err != nil {
		return errors.Trace(err)
	}
	return nil
}

// writeChartFile writes a chart file to disk
func writeChartFile(dest string, v interface{}) error {
	data, err := yaml.Marshal(v)
	if err != nil {
		return errors.Trace(err)
	}
	return ioutil.WriteFile(dest, data, 0644)
}

// hashDeps generates a hash of the dependencies.
//
// This should be used only to compare against another hash generated by this
// function.
func hashDeps(req, lock []*chart.Dependency) (string, error) {
	data, err := json.Marshal([2][]*chart.Dependency{req, lock})
	if err != nil {
		return "", err
	}
	s, err := provenance.Digest(bytes.NewBuffer(data))
	return "sha256:" + s, err
}

// getDependencyRepoURL calculates and return the proper URL to be used in dependencies files
func getDependencyRepoURL(targetRepo *api.Repo) (string, error) {
	repoUrl := targetRepo.GetUrl()
	if targetRepo.GetKind() == api.Kind_OCI {
		parseUrl, err := url.Parse(repoUrl)
		if err != nil {
			return "", errors.Trace(err)
		}
		parseUrl.Scheme = "oci"
		repoUrl = parseUrl.String()
	}
	return repoUrl, nil
}
