// Copyright 2020 The Operator-SDK Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package bundle

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/operator-framework/api/pkg/operators/v1alpha1"
	"github.com/operator-framework/operator-registry/alpha/action"
	declarativeconfig "github.com/operator-framework/operator-registry/alpha/declcfg"
	"github.com/operator-framework/operator-registry/pkg/containertools"
	registrybundle "github.com/operator-framework/operator-registry/pkg/lib/bundle"
	registryutil "github.com/operator-framework/operator-sdk/internal/registry"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/pflag"

	"github.com/operator-framework/operator-sdk/internal/olm/operator"
	"github.com/operator-framework/operator-sdk/internal/olm/operator/registry"
)

type Install struct {
	BundleImage string

	*registry.IndexImageCatalogCreator
	*registry.OperatorInstaller

	cfg *operator.Configuration
}

type FBCContext struct {
	BundleImage       string
	Package           string
	DefaultChannel    string
	FBCName           string
	FBCDirPath        string
	FBCDirName        string
	ChannelSchema     string
	ChannelName       string
	ChannelEntries    []declarativeconfig.ChannelEntry
	DescriptionReader io.Reader
}

func NewInstall(cfg *operator.Configuration) Install {
	i := Install{
		OperatorInstaller: registry.NewOperatorInstaller(cfg),
		cfg:               cfg,
	}
	i.IndexImageCatalogCreator = registry.NewIndexImageCatalogCreator(cfg)
	i.CatalogCreator = i.IndexImageCatalogCreator
	return i
}

func (i *Install) BindFlags(fs *pflag.FlagSet) {
	fs.StringVar(&i.IndexImage, "index-image", registry.DefaultIndexImage, "index image in which to inject bundle")
	fs.Var(&i.InstallMode, "install-mode", "install mode")

	// --mode is hidden so only users who know what they're doing can alter add mode.
	fs.StringVar((*string)(&i.BundleAddMode), "mode", "", "mode to use for adding bundle to index")
	_ = fs.MarkHidden("mode")

	i.IndexImageCatalogCreator.BindFlags(fs)
}

func (i Install) Run(ctx context.Context) (*v1alpha1.ClusterServiceVersion, error) {
	if err := i.setup(ctx); err != nil {
		return nil, err
	}
	return i.InstallOperator(ctx)
}

func (i *Install) setup(ctx context.Context) error {
	// Validate add mode in case it was set by a user.
	if i.BundleAddMode != "" {
		if err := i.BundleAddMode.Validate(); err != nil {
			return err
		}
	}

	//if user sets --skip-tls then set --use-http to true as --skip-tls is deprecated
	if i.SkipTLS {
		i.UseHTTP = true
	}

	// Load bundle labels and set label-dependent values.
	labels, bundle, err := operator.LoadBundle(ctx, i.BundleImage, i.SkipTLSVerify, i.UseHTTP)
	if err != nil {
		return err
	}
	csv := bundle.CSV

	if err := i.InstallMode.CheckCompatibility(csv, i.cfg.Namespace); err != nil {
		return err
	}

	var declcfg *declarativeconfig.DeclarativeConfig

	directoryName := filepath.Join("/tmp", strings.Split(csv.Name, ".")[0]+"-index")
	fileName := filepath.Join(directoryName, "testFBC")

	catalogLabels, err := registryutil.GetImageLabels(ctx, nil, i.IndexImageCatalogCreator.IndexImage, false)
	if err != nil {
		return fmt.Errorf("get index image labels: %v", err)
	}

	_, hasDBLabel := catalogLabels[containertools.DbLocationLabel]
	_, hasFBCLabel := catalogLabels[containertools.ConfigsLocationLabel]

	// handle both SQLite based and FBC based images.
	if hasDBLabel || hasFBCLabel {
		if i.IndexImageCatalogCreator.IndexImage != registry.DefaultIndexImage {
			declcfg, err = addBundleToIndexImage(i.IndexImageCatalogCreator.IndexImage, i.BundleImage)
			if err != nil {
				log.Errorf("error in rendering index image: %v", err)
				return err
			}

			log.Infof("Rendered a File-Based Catalog of the Index Image")
		}
	}

	if i.IndexImageCatalogCreator.IndexImage == registry.DefaultIndexImage {
		// if the index image is a default index image i.e the user did not provide an index image, then we create a file based catalog.
		bundleChannel := strings.Split(labels[registrybundle.ChannelsLabel], ",")[0]
		// FBC variables
		f := &FBCContext{
			BundleImage:    i.BundleImage,
			FBCDirName:     directoryName,
			FBCName:        fileName,
			Package:        labels[registrybundle.PackageLabel],
			DefaultChannel: bundleChannel,
			ChannelSchema:  "olm.channel",
			ChannelName:    bundleChannel,
		}

		// create entries for channel blob
		entries := []declarativeconfig.ChannelEntry{
			{
				Name: csv.Name,
			},
		}
		f.ChannelEntries = entries

		log.Infof("Generating a File-Based Catalog")

		// generate an FBC
		declcfg, err = f.createFBC()
		if err != nil {
			log.Errorf("error creating a minimal FBC: %v", err)
			return err
		}
	}

	// validate the declarative config
	if err = validateFBC(declcfg); err != nil {
		log.Errorf("error validating the generated FBC: %v", err)
		return err
	}

	// convert declarative config to string
	content, err := stringifyDecConfig(declcfg)

	if err != nil {
		log.Errorf("error converting declarative config to string: %v", err)
		return err
	}

	if content == "" {
		return errors.New("File-Based Catalog contents cannot be empty")
	}

	log.Infof("Generated a valid File-Based Catalog")

	i.OperatorInstaller.PackageName = labels[registrybundle.PackageLabel]
	i.OperatorInstaller.CatalogSourceName = operator.CatalogNameForPackage(i.OperatorInstaller.PackageName)
	i.OperatorInstaller.StartingCSV = csv.Name
	i.OperatorInstaller.SupportedInstallModes = operator.GetSupportedInstallModes(csv.Spec.InstallModes)
	i.OperatorInstaller.Channel = strings.Split(labels[registrybundle.ChannelsLabel], ",")[0]

	i.IndexImageCatalogCreator.PackageName = i.OperatorInstaller.PackageName
	i.IndexImageCatalogCreator.BundleImage = i.BundleImage
	i.IndexImageCatalogCreator.FBCcontent = content
	i.IndexImageCatalogCreator.FBCdir = directoryName
	i.IndexImageCatalogCreator.FBCfile = fileName

	return nil
}

// addBundleToIndexImage adds the bundle to an existing index image if the bundle is not already present in the index image.
func addBundleToIndexImage(indexImage, bundleImage string) (*declarativeconfig.DeclarativeConfig, error) {
	var bundleDeclConfig *declarativeconfig.DeclarativeConfig
	render := action.Render{
		Refs: []string{indexImage},
	}

	log.Infof("Rendering a File-Based Catalog of the Index Image")

	imageDeclConfig, err := render.Run(context.TODO())
	if err != nil {
		return nil, err
	}

	// render the bundle image to a declarative config.
	render = action.Render{
		Refs: []string{bundleImage},
	}

	bundleDeclConfig, err = render.Run(context.TODO())
	if err != nil {
		log.Errorf("error in rendering the bundle image: %v", err)
		return nil, err
	}

	if len(bundleDeclConfig.Bundles) < 0 {
		log.Errorf("error in rendering the correct number of bundles: %v", err)
		return nil, err
	}

	// check if the package blob already exists in the image
	packageNotPresent := true
	if len(bundleDeclConfig.Packages) > 0 {
		for _, packageName := range imageDeclConfig.Packages {
			if reflect.DeepEqual(packageName, bundleDeclConfig.Packages[0]) {
				packageNotPresent = false
				break
			}
		}
	}

	if packageNotPresent && len(bundleDeclConfig.Bundles) > 0 && len(bundleDeclConfig.Channels) > 0 {
		imageDeclConfig.Packages = append(imageDeclConfig.Packages, bundleDeclConfig.Packages[0])
		if len(bundleDeclConfig.Bundles) > 0 {
			imageDeclConfig.Bundles = append(imageDeclConfig.Bundles, bundleDeclConfig.Bundles[0])
		}
		if len(bundleDeclConfig.Channels) > 0 {
			imageDeclConfig.Channels = append(imageDeclConfig.Channels, bundleDeclConfig.Channels[0])
		}

		if len(bundleDeclConfig.Others) > 0 {
			imageDeclConfig.Others = append(imageDeclConfig.Others, bundleDeclConfig.Others[0])
		}
	}

	return imageDeclConfig, nil
}

//createFBC generates an FBC by creating bundle, package and channel blobs.
func (f *FBCContext) createFBC() (*declarativeconfig.DeclarativeConfig, error) {

	var (
		declcfg        *declarativeconfig.DeclarativeConfig
		declcfgpackage *declarativeconfig.Package
		err            error
	)

	render := action.Render{
		Refs: []string{f.BundleImage},
	}

	// generate bundles by rendering the bundle objects.
	declcfg, err = render.Run(context.TODO())
	if err != nil {
		log.Errorf("error in rendering the bundle image: %v", err)
		return nil, err
	}

	if len(declcfg.Bundles) < 0 {
		log.Errorf("error in rendering the correct number of bundles: %v", err)
		return nil, err
	}
	// validate length of bundles and add them to declcfg.Bundles.
	if len(declcfg.Bundles) == 1 {
		declcfg.Bundles = []declarativeconfig.Bundle{*&declcfg.Bundles[0]}
	} else {
		return nil, errors.New("error in expected length of bundles")
	}

	// init packages
	init := action.Init{
		Package:           f.Package,
		DefaultChannel:    f.ChannelName,
		DescriptionReader: f.DescriptionReader,
	}

	// generate packages
	declcfgpackage, err = init.Run()
	if err != nil {
		log.Errorf("error in generating packages for the FBC: %v", err)
		return nil, err
	}
	declcfg.Packages = []declarativeconfig.Package{*declcfgpackage}

	// generate channels
	channel := declarativeconfig.Channel{
		Schema:  f.ChannelSchema,
		Name:    f.ChannelName,
		Package: f.Package,
		Entries: f.ChannelEntries,
	}

	declcfg.Channels = []declarativeconfig.Channel{channel}

	return declcfg, nil
}

// stringifyDecConfig writes the generated declarative config to a string.
func stringifyDecConfig(declcfg *declarativeconfig.DeclarativeConfig) (string, error) {
	var buf bytes.Buffer
	err := declarativeconfig.WriteJSON(*declcfg, &buf)
	if err != nil {
		log.Errorf("error writing to JSON encoder: %v", err)
		return "", err
	}

	return string(buf.Bytes()), nil
}

// validateFBC converts the generated declarative config to a model and validates it.
func validateFBC(declcfg *declarativeconfig.DeclarativeConfig) error {
	// convert declarative config to model
	FBCmodel, err := declarativeconfig.ConvertToModel(*declcfg)
	if err != nil {
		log.Errorf("error converting the declarative config to mode: %v", err)
		return err
	}

	if err = FBCmodel.Validate(); err != nil {
		log.Errorf("error validating the FBC: %v", err)
		return err
	}

	return nil
}
