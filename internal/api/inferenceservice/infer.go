// Copyright 2025 Google LLC
// SPDX-License-Identifier: Apache-2.0

package inferenceservice

import (
	"context"
	"log"

	"github.com/google/oss-rebuild/internal/api"
	"github.com/google/oss-rebuild/internal/api/cratesregistryservice"
	"github.com/google/oss-rebuild/internal/gitcache"
	"github.com/google/oss-rebuild/internal/gitx"
	"github.com/google/oss-rebuild/internal/httpx"
	"github.com/google/oss-rebuild/internal/uri"
	"github.com/google/oss-rebuild/pkg/rebuild/meta"
	"github.com/google/oss-rebuild/pkg/rebuild/rebuild"
	"github.com/google/oss-rebuild/pkg/rebuild/schema"
	"github.com/pkg/errors"
	"google.golang.org/grpc/codes"
)

// The strategy is build from the strategy hint, and represents exactly one of the valid strategies
func doInfer(
	ctx context.Context,
	rebuilder rebuild.Rebuilder,
	target rebuild.Target,
	mux rebuild.RegistryMux,
	initialRebuildStrategy rebuild.Strategy,
	ropt *gitx.RepositoryOptions,
) (rebuild.Strategy, error) {
	var repo string
	// First, we get the repo. In my case, this is usually just given by the hint.
	if locationHint, ok := initialRebuildStrategy.(*rebuild.LocationHint); ok && locationHint != nil && locationHint.Repo != "" {
		var err error
		repo, err = uri.CanonicalizeRepoURI(locationHint.Repo)
		if err != nil {
			return nil, errors.Wrap(err, "canonicalizing repo hint")
		}
	} else if strategyNameHint, ok := initialRebuildStrategy.(*rebuild.CommitInferenceStrategyHint); ok && strategyNameHint != nil && strategyNameHint.Repo != "" {
		var err error
		repo, err = uri.CanonicalizeRepoURI(strategyNameHint.Repo)
		if err != nil {
			return nil, errors.Wrap(err, "canonicalizing repo hint")
		}
	} else {
		var err error
		repo, err = rebuilder.InferRepo(ctx, target, mux)
		if err != nil {
			return nil, err
		}
	}
	// Now we clone the repo
	rcfg, err := rebuilder.CloneRepo(ctx, target, repo, ropt)
	if err != nil {
		return nil, err
	}
	// Then we infer the actual strategy -- this is the main inference we do
	// The rebuilder is given by the calling function `Infer`, chosen according to the ecosystem
	// from `pkg/rebuild/meta/meta.go::AllRebuilders`
	rebuildStrategy, err := rebuilder.InferStrategy(ctx, target, mux, &rcfg, initialRebuildStrategy)
	if err != nil {
		return nil, err
	}
	return rebuildStrategy, nil
}

type InferDeps struct {
	HTTPClient         httpx.BasicClient
	GitCache           *gitcache.Client
	RepoOptF           func() *gitx.RepositoryOptions
	CratesRegistryStub api.StubT[cratesregistryservice.FindRegistryCommitRequest, cratesregistryservice.FindRegistryCommitResponse]
}

func Infer(ctx context.Context, req schema.InferenceRequest, deps *InferDeps) (*schema.StrategyOneOf, error) {
	if req.LocationHint() != nil && req.LocationHint().Ref == "" && req.LocationHint().Dir != "" {
		return nil, api.AsStatus(codes.Unimplemented, errors.New("location hint dir without ref not implemented"))
	}
	if req.LocationHint() != nil && req.LocationHint().Repo == "" {
		return nil, api.AsStatus(codes.InvalidArgument, errors.New("location hint without repo is not supported"))
	}
	repoOpt := deps.RepoOptF()
	if repoOpt.Worktree == nil {
		return nil, api.AsStatus(codes.Internal, errors.New("filesystem not provided"))
	}
	if repoOpt.Storer == nil {
		return nil, api.AsStatus(codes.Internal, errors.New("git storage not provided"))
	}
	if deps.GitCache != nil {
		ctx = context.WithValue(ctx, rebuild.RepoCacheClientID, deps.GitCache)
	}
	ctx = context.WithValue(ctx, rebuild.HTTPBasicClientID, deps.HTTPClient)
	if deps.CratesRegistryStub != nil {
		ctx = context.WithValue(ctx, rebuild.CratesRegistryStubID, deps.CratesRegistryStub)
	}
	mux := meta.NewRegistryMux(deps.HTTPClient)
	var strategy rebuild.Strategy
	target := rebuild.Target{
		Ecosystem: req.Ecosystem,
		Package:   req.Package,
		Version:   req.Version,
		Artifact:  req.Artifact,
	}
	rebuilder, ok := meta.AllRebuilders[req.Ecosystem]
	if !ok {
		return nil, api.AsStatus(codes.InvalidArgument, errors.New("unsupported ecosystem"))
	}
	if req.StrategyHint != nil {
		var err error
		strategy, err = req.StrategyHint.Strategy()
		if err != nil {
			return nil, api.AsStatus(codes.InvalidArgument, errors.Wrap(err, "invalid strategy hint"))
		}
	}
	strategy, err := doInfer(ctx, rebuilder, target, mux, strategy, repoOpt)
	if err != nil {
		log.Printf("No inference for [pkg=%s, version=%v]: %v\n", req.Package, req.Version, err)
		return nil, api.AsStatus(codes.Internal, errors.Wrap(err, "failed to infer strategy"))
	}
	oneof := schema.NewStrategyOneOf(strategy)
	return &oneof, nil
}
