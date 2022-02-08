/*
Copyright 2022 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package promoter

//go:generate go run github.com/maxbrunsfeld/counterfeiter/v6 -generate

import (
	"github.com/pkg/errors"
	reg "sigs.k8s.io/promo-tools/v3/legacy/dockerregistry"
	"sigs.k8s.io/promo-tools/v3/legacy/stream"
)

var AllowedOutputFormats = []string{
	"csv",
	"yaml",
}

type Promoter struct {
	Options *Options
	impl    promoterImplementation
}

func New() *Promoter {
	return &Promoter{
		Options: DefaultOptions,
		impl:    &defaultPromoterImplementation{},
	}
}

//counterfeiter:generate . promoterImplementation

// promoterImplementation handles all the functionality in the promoter
// modes of operation.
type promoterImplementation interface {
	// General methods common to all modes of the promoter
	ValidateOptions(*Options) error
	ActivateServiceAccounts(*Options) error

	// Methods for promotion mode:
	ParseManifests(*Options) ([]reg.Manifest, error)
	MakeSyncContext(*Options, []reg.Manifest) (*reg.SyncContext, error)
	GetPromotionEdges(*reg.SyncContext, []reg.Manifest) (map[reg.PromotionEdge]interface{}, error)
	MakeProducerFunction(bool) streamProducerFunc
	PromoteImages(*reg.SyncContext, map[reg.PromotionEdge]interface{}, streamProducerFunc) error

	// Methods for snapshot mode:
	GetSnapshotSourceRegistry(*Options) (*reg.RegistryContext, error)
	GetSnapshotManifests(*Options) ([]reg.Manifest, error)
	AppendManifestToSnapshot(*Options, []reg.Manifest) ([]reg.Manifest, error)
	GetRegistryImageInventory(*Options, []reg.Manifest) (reg.RegInvImage, error)
	Snapshot(*Options, reg.RegInvImage) error

	// Methods for image vulnerability scans:
	ScanEdges(*Options, *reg.SyncContext, map[reg.PromotionEdge]interface{}) error

	// Methods for manifest list verification:
	ValidateManifestLists(opts *Options) error
}

// streamProducerFunc is a function that gets the required fields to
// construct a promotion stream producer
type streamProducerFunc func(
	srcRegistry reg.RegistryName, srcImageName reg.ImageName,
	destRC reg.RegistryContext, imageName reg.ImageName,
	digest reg.Digest, tag reg.Tag, tp reg.TagOp,
) stream.Producer

// PromoteImages is the main method for image promotion
// it runs by taking all its parameters from a set of options.
func (p *Promoter) PromoteImages(opts *Options) (err error) {
	// Validate the options. Perhaps another image-specific
	// validation function may be needed.
	if err := p.impl.ValidateOptions(opts); err != nil {
		return errors.Wrap(err, "validating options")
	}

	if err := p.impl.ActivateServiceAccounts(opts); err != nil {
		return errors.Wrap(err, "activating service accounts")
	}

	mfests, err := p.impl.ParseManifests(opts)
	if err != nil {
		return errors.Wrap(err, "parsing manifests")
	}

	sc, err := p.impl.MakeSyncContext(opts, mfests)
	if err != nil {
		return errors.Wrap(err, "creating sync context")
	}

	promotionEdges, err := p.impl.GetPromotionEdges(sc, mfests)
	if err != nil {
		return errors.Wrap(err, "filtering edges")
	}

	// MakeProducer
	producerFunc := p.impl.MakeProducerFunction(sc.UseServiceAccount)

	// If parseOnly from the original cli.Run fn is kept, this is where it goes

	return errors.Wrap(
		p.impl.PromoteImages(sc, promotionEdges, producerFunc),
		"running promotion",
	)
}

// Snapshot runs the steps to output a representation in json or yaml of a registry
func (p *Promoter) Snapshot(opts *Options) (err error) {
	if err := p.impl.ValidateOptions(opts); err != nil {
		return errors.Wrap(err, "validating options")
	}

	if err := p.impl.ActivateServiceAccounts(opts); err != nil {
		return errors.Wrap(err, "activating service accounts")
	}

	mfests, err := p.impl.GetSnapshotManifests(opts)
	if err != nil {
		return errors.Wrap(err, "getting snapshot manifests")
	}

	mfests, err = p.impl.AppendManifestToSnapshot(opts, mfests)
	if err != nil {
		return errors.Wrap(err, "adding the specified manifest to the snapshot context")
	}

	rii, err := p.impl.GetRegistryImageInventory(opts, mfests)
	if err != nil {
		return errors.Wrap(err, "getting registry image inventory")
	}

	return errors.Wrap(p.impl.Snapshot(opts, rii), "generating snapshot")
}

// SecurityScan runs just like an image promotion, but instead of
// actually copying the new detected images, it will run a vulnerability
// scan on them
func (p *Promoter) SecurityScan(opts *Options) error {
	if err := p.impl.ValidateOptions(opts); err != nil {
		return errors.Wrap(err, "validating options")
	}

	if err := p.impl.ActivateServiceAccounts(opts); err != nil {
		return errors.Wrap(err, "activating service accounts")
	}

	mfests, err := p.impl.ParseManifests(opts)
	if err != nil {
		return errors.Wrap(err, "parsing manifests")
	}

	sc, err := p.impl.MakeSyncContext(opts, mfests)
	if err != nil {
		return errors.Wrap(err, "creating sync context")
	}

	promotionEdges, err := p.impl.GetPromotionEdges(sc, mfests)
	if err != nil {
		return errors.Wrap(err, "filtering edges")
	}

	return errors.Wrap(
		p.impl.ScanEdges(opts, sc, promotionEdges),
		"running vulnerability scan",
	)
}

// CheckManifestLists is a mode that just checks manifests
// and exists.
func (p *Promoter) CheckManifestLists(opts *Options) error {
	if err := p.impl.ValidateOptions(opts); err != nil {
		return errors.Wrap(err, "validating options")
	}

	if err := p.impl.ActivateServiceAccounts(opts); err != nil {
		return errors.Wrap(err, "activating service accounts")
	}

	return errors.Wrap(
		p.impl.ValidateManifestLists(opts), "checking manifest lists",
	)
}

type defaultPromoterImplementation struct{}

func (di *defaultPromoterImplementation) ValidateManifestLists(opts *Options) error {
	registry := reg.RegistryName(opts.Repository)
	images := make([]reg.ImageWithDigestSlice, 0)

	if err := reg.ParseSnapshot(opts.CheckManifestLists, &images); err != nil {
		return errors.Wrap(err, "parsing snapshot")
	}

	imgs, err := reg.FilterParentImages(registry, &images)
	if err != nil {
		return errors.Wrap(err, "filtering parent images")
	}

	reg.ValidateParentImages(registry, imgs)
	printSection("FINISHED (CHECKING MANIFESTS)", true)
	return nil
}
