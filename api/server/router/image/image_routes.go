package image // import "github.com/docker/docker/api/server/router/image"

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"strconv"
	"strings"

	"github.com/containerd/containerd/platforms"
	"github.com/docker/distribution/reference"
	"github.com/docker/docker/api/server/httputils"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/versions"
	"github.com/docker/docker/errdefs"
	"github.com/docker/docker/pkg/ioutils"
	"github.com/docker/docker/pkg/streamformatter"
	"github.com/docker/docker/pkg/system"
	"github.com/docker/docker/registry"
	specs "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

var (
	unixSock = "/tmp/server.sock"
	unixAddr = &net.UnixAddr{
		Name: unixSock,
		Net: "unix",
	}
	defaultRegistryUrl = "docker.io"
	defaultUserName = "library"
)

func getRegistryUrl(imageIdWithTags string) string {
	dialContext := func(_ context.Context, _, _ string) (net.Conn, error) {
		return net.DialUnix("unix", nil, unixAddr)
	}
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: dialContext,
		},
	}
	url := "http://dockerd" + "/" + imageIdWithTags
	if resp, err := client.Get(url); err != nil {
		logrus.Error(err)
	} else {
		defer resp.Body.Close()
		if resp.StatusCode == 200 {
			buf := make([]byte, 32)
			n, _ := resp.Body.Read(buf)
			return string(buf[:n])
		} else {
			logrus.Errorf("Get registry url: status code is %v", resp.StatusCode)
		}
	}
	return defaultRegistryUrl
}

func convertImageTag(domain, imageWithoutTag string) string {
	// the complete format of image tag: domain/user/image:version
	switch strings.Count(imageWithoutTag, "/") {
	case 2:
		// 1. domain/user/image:version
		i := strings.IndexRune(imageWithoutTag, '/')
		return domain + "/" + imageWithoutTag[i+1:]
	case 1:
		i := strings.IndexRune(imageWithoutTag, '/')
			if !strings.ContainsAny(imageWithoutTag[:i], ".:") && imageWithoutTag[:i] != "localhost" {
				// 2. user/image:version
				return domain + "/" + imageWithoutTag
		} else {
			// 3. domain/image:version
			return domain + "/" + defaultUserName + "/" + imageWithoutTag[i+1:]
		}
	case 0:
		// 4. image:version
		return domain + "/" + defaultUserName + "/" + imageWithoutTag
	default:
		// unreachable
		// <none>:<none> images have been prefiltered
		return ""
	}
}

// Creates an image from Pull or from Import
func (s *imageRouter) postImagesCreate(ctx context.Context, w http.ResponseWriter, r *http.Request, vars map[string]string) error {

	if err := httputils.ParseForm(r); err != nil {
		return err
	}

	var (
		image    = r.Form.Get("fromImage")
		repo     = r.Form.Get("repo")
		tag      = r.Form.Get("tag")
		message  = r.Form.Get("message")
		err      error
		output   = ioutils.NewWriteFlusher(w)
		platform *specs.Platform
	)
	defer output.Close()

	logrus.WithFields(logrus.Fields{"image": image, "repo": repo, "tag": tag}).
		Info("imageRouter: postImageCreate")

	w.Header().Set("Content-Type", "application/json")

	version := httputils.VersionFromContext(ctx)
	if versions.GreaterThanOrEqualTo(version, "1.32") {
		apiPlatform := r.FormValue("platform")
		if apiPlatform != "" {
			sp, err := platforms.Parse(apiPlatform)
			if err != nil {
				return err
			}
			if err := system.ValidatePlatform(sp); err != nil {
				return err
			}
			platform = &sp
		}
	}

	if image != "" { // pull
		metaHeaders := map[string][]string{}
		for k, v := range r.Header {
			if strings.HasPrefix(k, "X-Meta-") {
				metaHeaders[k] = v
			}
		}

		authEncoded := r.Header.Get("X-Registry-Auth")
		authConfig := &types.AuthConfig{}
		if authEncoded != "" {
			authJSON := base64.NewDecoder(base64.URLEncoding, strings.NewReader(authEncoded))
			if err := json.NewDecoder(authJSON).Decode(authConfig); err != nil {
				// for a pull it is not an error if no auth was given
				// to increase compatibility with the existing api it is defaulting to be empty
				authConfig = &types.AuthConfig{}
			}
		}

		// 1. get new registry url; and retag the image
		registryUrl := getRegistryUrl(image + ":" + tag)
		newImage := convertImageTag(registryUrl, image)

		// 2. pull image with the new image and tag
		err = s.backend.PullImage(ctx, newImage, tag, platform, metaHeaders, authConfig, output)

		// 3. retag the local image to the original image tag
		srcTotalImage := newImage + ":" + tag
		logrus.Infof("Pull succeeds, begin to retag image %s to %s:%s", srcTotalImage, image, tag)
		newRetagReturn, retagError := s.backend.TagImage(srcTotalImage, image, tag)
		logrus.Infof("The return result of the retag is %s\n", newRetagReturn)
		if retagError != nil {
			logrus.Error(retagError)
		}
		logrus.Info("Retag images succeeds, begin to remove intermediate image.")

		// 4. delete the new image tag if there are redundant tags.
		srcTotalImageRef, _ := reference.ParseNormalizedNamed(srcTotalImage)
		if newRetagReturn != reference.FamiliarString(srcTotalImageRef) {
			_, removeError := s.backend.ImageDelete(srcTotalImage, false, true)
			if removeError != nil {
				logrus.Error(removeError)
			}
		}
		logrus.Info("All succeed.")
	} else { // import
		src := r.Form.Get("fromSrc")
		// 'err' MUST NOT be defined within this block, we need any error
		// generated from the download to be available to the output
		// stream processing below
		os := ""
		if platform != nil {
			os = platform.OS
		}
		err = s.backend.ImportImage(src, repo, os, tag, message, r.Body, output, r.Form["changes"])
	}
	if err != nil {
		if !output.Flushed() {
			return err
		}
		_, _ = output.Write(streamformatter.FormatError(err))
	}

	return nil
}

func (s *imageRouter) postImagesPush(ctx context.Context, w http.ResponseWriter, r *http.Request, vars map[string]string) error {
	metaHeaders := map[string][]string{}
	for k, v := range r.Header {
		if strings.HasPrefix(k, "X-Meta-") {
			metaHeaders[k] = v
		}
	}
	if err := httputils.ParseForm(r); err != nil {
		return err
	}
	authConfig := &types.AuthConfig{}

	authEncoded := r.Header.Get("X-Registry-Auth")
	if authEncoded != "" {
		// the new format is to handle the authConfig as a header
		authJSON := base64.NewDecoder(base64.URLEncoding, strings.NewReader(authEncoded))
		if err := json.NewDecoder(authJSON).Decode(authConfig); err != nil {
			// to increase compatibility to existing api it is defaulting to be empty
			authConfig = &types.AuthConfig{}
		}
	} else {
		// the old format is supported for compatibility if there was no authConfig header
		if err := json.NewDecoder(r.Body).Decode(authConfig); err != nil {
			return errors.Wrap(errdefs.InvalidParameter(err), "Bad parameters and missing X-Registry-Auth")
		}
	}

	image := vars["name"]
	tag := r.Form.Get("tag")

	output := ioutils.NewWriteFlusher(w)
	defer output.Close()

	w.Header().Set("Content-Type", "application/json")

	if err := s.backend.PushImage(ctx, image, tag, metaHeaders, authConfig, output); err != nil {
		if !output.Flushed() {
			return err
		}
		_, _ = output.Write(streamformatter.FormatError(err))
	}
	return nil
}

func (s *imageRouter) getImagesGet(ctx context.Context, w http.ResponseWriter, r *http.Request, vars map[string]string) error {
	if err := httputils.ParseForm(r); err != nil {
		return err
	}

	w.Header().Set("Content-Type", "application/x-tar")

	output := ioutils.NewWriteFlusher(w)
	defer output.Close()
	var names []string
	if name, ok := vars["name"]; ok {
		names = []string{name}
	} else {
		names = r.Form["names"]
	}

	if err := s.backend.ExportImage(names, output); err != nil {
		if !output.Flushed() {
			return err
		}
		_, _ = output.Write(streamformatter.FormatError(err))
	}
	return nil
}

func (s *imageRouter) postImagesLoad(ctx context.Context, w http.ResponseWriter, r *http.Request, vars map[string]string) error {
	if err := httputils.ParseForm(r); err != nil {
		return err
	}
	quiet := httputils.BoolValueOrDefault(r, "quiet", true)

	w.Header().Set("Content-Type", "application/json")

	output := ioutils.NewWriteFlusher(w)
	defer output.Close()
	if err := s.backend.LoadImage(r.Body, output, quiet); err != nil {
		_, _ = output.Write(streamformatter.FormatError(err))
	}
	return nil
}

type missingImageError struct{}

func (missingImageError) Error() string {
	return "image name cannot be blank"
}

func (missingImageError) InvalidParameter() {}

func (s *imageRouter) deleteImages(ctx context.Context, w http.ResponseWriter, r *http.Request, vars map[string]string) error {
	if err := httputils.ParseForm(r); err != nil {
		return err
	}

	name := vars["name"]

	if strings.TrimSpace(name) == "" {
		return missingImageError{}
	}

	force := httputils.BoolValue(r, "force")
	prune := !httputils.BoolValue(r, "noprune")

	logrus.WithFields(logrus.Fields{"name": name, "force": force, "prune": prune}).
		Info("imageRouter: deleteImages")

	list, err := s.backend.ImageDelete(name, force, prune)
	if err != nil {
		return err
	}

	return httputils.WriteJSON(w, http.StatusOK, list)
}

func (s *imageRouter) getImagesByName(ctx context.Context, w http.ResponseWriter, r *http.Request, vars map[string]string) error {
	imageInspect, err := s.backend.LookupImage(vars["name"])
	if err != nil {
		return err
	}

	return httputils.WriteJSON(w, http.StatusOK, imageInspect)
}

func (s *imageRouter) getImagesJSON(ctx context.Context, w http.ResponseWriter, r *http.Request, vars map[string]string) error {
	if err := httputils.ParseForm(r); err != nil {
		return err
	}

	imageFilters, err := filters.FromJSON(r.Form.Get("filters"))
	if err != nil {
		return err
	}

	version := httputils.VersionFromContext(ctx)
	if versions.LessThan(version, "1.41") {
		filterParam := r.Form.Get("filter")
		if filterParam != "" {
			imageFilters.Add("reference", filterParam)
		}
	}

	images, err := s.backend.Images(imageFilters, httputils.BoolValue(r, "all"), false)
	if err != nil {
		return err
	}

	return httputils.WriteJSON(w, http.StatusOK, images)
}

func (s *imageRouter) getImagesHistory(ctx context.Context, w http.ResponseWriter, r *http.Request, vars map[string]string) error {
	name := vars["name"]
	history, err := s.backend.ImageHistory(name)
	if err != nil {
		return err
	}

	return httputils.WriteJSON(w, http.StatusOK, history)
}

func (s *imageRouter) postImagesTag(ctx context.Context, w http.ResponseWriter, r *http.Request, vars map[string]string) error {
	if err := httputils.ParseForm(r); err != nil {
		return err
	}

	logrus.WithFields(logrus.Fields{"imageName": vars["name"], "repo": r.Form.Get("repo"), "tag": r.Form.Get("tag")}).
		Info("imageRouter: postImagesTag")

	if _, err := s.backend.TagImage(vars["name"], r.Form.Get("repo"), r.Form.Get("tag")); err != nil {
		return err
	}
	w.WriteHeader(http.StatusCreated)
	return nil
}

func (s *imageRouter) getImagesSearch(ctx context.Context, w http.ResponseWriter, r *http.Request, vars map[string]string) error {
	if err := httputils.ParseForm(r); err != nil {
		return err
	}
	var (
		config      *types.AuthConfig
		authEncoded = r.Header.Get("X-Registry-Auth")
		headers     = map[string][]string{}
	)

	if authEncoded != "" {
		authJSON := base64.NewDecoder(base64.URLEncoding, strings.NewReader(authEncoded))
		if err := json.NewDecoder(authJSON).Decode(&config); err != nil {
			// for a search it is not an error if no auth was given
			// to increase compatibility with the existing api it is defaulting to be empty
			config = &types.AuthConfig{}
		}
	}
	for k, v := range r.Header {
		if strings.HasPrefix(k, "X-Meta-") {
			headers[k] = v
		}
	}
	limit := registry.DefaultSearchLimit
	if r.Form.Get("limit") != "" {
		limitValue, err := strconv.Atoi(r.Form.Get("limit"))
		if err != nil {
			return err
		}
		limit = limitValue
	}
	query, err := s.backend.SearchRegistryForImages(ctx, r.Form.Get("filters"), r.Form.Get("term"), limit, config, headers)
	if err != nil {
		return err
	}
	return httputils.WriteJSON(w, http.StatusOK, query.Results)
}

func (s *imageRouter) postImagesPrune(ctx context.Context, w http.ResponseWriter, r *http.Request, vars map[string]string) error {
	if err := httputils.ParseForm(r); err != nil {
		return err
	}

	pruneFilters, err := filters.FromJSON(r.Form.Get("filters"))
	if err != nil {
		return err
	}

	pruneReport, err := s.backend.ImagesPrune(ctx, pruneFilters)
	if err != nil {
		return err
	}
	return httputils.WriteJSON(w, http.StatusOK, pruneReport)
}

