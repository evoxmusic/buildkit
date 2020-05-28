package containerimage

import (
	"context"
	"encoding/json"
	"runtime"
	"sync"
	"time"

	"github.com/containerd/containerd/content"
	"github.com/containerd/containerd/diff"
	"github.com/containerd/containerd/images"
	"github.com/containerd/containerd/leases"
	"github.com/containerd/containerd/platforms"
	"github.com/containerd/containerd/remotes/docker"
	"github.com/docker/docker/errdefs"
	"github.com/moby/buildkit/cache"
	"github.com/moby/buildkit/client/llb"
	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/snapshot"
	"github.com/moby/buildkit/solver"
	"github.com/moby/buildkit/source"
	"github.com/moby/buildkit/util/flightcontrol"
	"github.com/moby/buildkit/util/imageutil"
	"github.com/moby/buildkit/util/leaseutil"
	"github.com/moby/buildkit/util/progress"
	"github.com/moby/buildkit/util/progress/controller"
	"github.com/moby/buildkit/util/pull"
	"github.com/moby/buildkit/util/resolver"
	digest "github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/identity"
	specs "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
)

// TODO: break apart containerd specifics like contentstore so the resolver
// code can be used with any implementation

type SourceOpt struct {
	Snapshotter   snapshot.Snapshotter
	ContentStore  content.Store
	Applier       diff.Applier
	CacheAccessor cache.Accessor
	ImageStore    images.Store // optional
	RegistryHosts docker.RegistryHosts
	LeaseManager  leases.Manager
}

type Source struct {
	SourceOpt
	g flightcontrol.Group
}

var _ source.Source = &Source{}

func NewSource(opt SourceOpt) (*Source, error) {
	is := &Source{
		SourceOpt: opt,
	}

	return is, nil
}

func (is *Source) ID() string {
	return source.DockerImageScheme
}

func (is *Source) ResolveImageConfig(ctx context.Context, ref string, opt llb.ResolveImageConfigOpt, sm *session.Manager, g session.Group) (digest.Digest, []byte, error) {
	type t struct {
		dgst digest.Digest
		dt   []byte
	}
	key := ref
	if platform := opt.Platform; platform != nil {
		key += platforms.Format(*platform)
	}

	rm, err := source.ParseImageResolveMode(opt.ResolveMode)
	if err != nil {
		return "", nil, err
	}

	res, err := is.g.Do(ctx, key, func(ctx context.Context) (interface{}, error) {
		dgst, dt, err := imageutil.Config(ctx, ref, pull.NewResolver(g, pull.ResolverOpt{
			Hosts:      is.RegistryHosts,
			Auth:       resolver.NewSessionAuthenticator(sm, g),
			ImageStore: is.ImageStore,
			Mode:       rm,
			Ref:        ref,
		}), is.ContentStore, is.LeaseManager, opt.Platform)
		if err != nil {
			return nil, err
		}
		return &t{dgst: dgst, dt: dt}, nil
	})
	if err != nil {
		return "", nil, err
	}
	typed := res.(*t)
	return typed.dgst, typed.dt, nil
}

func (is *Source) Resolve(ctx context.Context, id source.Identifier, sm *session.Manager, vtx solver.Vertex) (source.SourceInstance, error) {
	imageIdentifier, ok := id.(*source.ImageIdentifier)
	if !ok {
		return nil, errors.Errorf("invalid image identifier %v", id)
	}

	platform := platforms.DefaultSpec()
	if imageIdentifier.Platform != nil {
		platform = *imageIdentifier.Platform
	}

	pullerUtil := &pull.Puller{
		ContentStore: is.ContentStore,
		Platform:     &platform,
		Src:          imageIdentifier.Reference,
	}
	p := &puller{
		CacheAccessor: is.CacheAccessor,
		LeaseManager:  is.LeaseManager,
		Puller:        pullerUtil,
		id:            imageIdentifier,
		ResolverOpt: pull.ResolverOpt{
			Hosts:      is.RegistryHosts,
			Auth:       resolver.NewSessionAuthenticator(sm, nil),
			ImageStore: is.ImageStore,
			Mode:       imageIdentifier.ResolveMode,
			Ref:        imageIdentifier.Reference.String(),
		},
		vtx: vtx,
	}
	return p, nil
}

type puller struct {
	CacheAccessor cache.Accessor
	LeaseManager  leases.Manager
	ResolverOpt   pull.ResolverOpt
	id            *source.ImageIdentifier
	vtx           solver.Vertex

	cacheKeyOnce     sync.Once
	cacheKeyErr      error
	releaseTmpLeases func(context.Context) error
	descHandlers     cache.DescHandlers
	manifest         *pull.PulledManifests
	manifestKey      string
	configKey        string
	*pull.Puller
}

func mainManifestKey(ctx context.Context, desc specs.Descriptor, platform *specs.Platform) (digest.Digest, error) {
	keyStruct := struct {
		Digest  digest.Digest
		OS      string
		Arch    string
		Variant string `json:",omitempty"`
	}{
		Digest: desc.Digest,
	}
	if platform != nil {
		keyStruct.OS = platform.OS
		keyStruct.Arch = platform.Architecture
		keyStruct.Variant = platform.Variant
	}

	dt, err := json.Marshal(keyStruct)
	if err != nil {
		return "", err
	}
	return digest.FromBytes(dt), nil
}

func (p *puller) CacheKey(ctx context.Context, g session.Group, index int) (cacheKey string, cacheOpts solver.CacheOpts, cacheDone bool, err error) {
	if p.Puller.Resolver == nil {
		p.Puller.Resolver = pull.NewResolver(g, p.ResolverOpt)
	} else {
		p.ResolverOpt.Auth.AddSession(g)
	}

	p.cacheKeyOnce.Do(func() {
		ctx, done, err := leaseutil.WithLease(ctx, p.LeaseManager, leases.WithExpiration(5*time.Minute), leaseutil.MakeTemporary)
		if err != nil {
			p.cacheKeyErr = err
			return
		}
		p.releaseTmpLeases = done
		imageutil.AddLease(p.releaseTmpLeases)
		defer func() {
			if p.cacheKeyErr != nil {
				p.releaseTmpLeases(ctx)
			}
		}()

		resolveProgressDone := oneOffProgress(ctx, "resolve "+p.Src.String())
		defer func() {
			resolveProgressDone(err)
		}()

		p.manifest, err = p.PullManifests(ctx)
		if err != nil {
			p.cacheKeyErr = err
			return
		}

		if len(p.manifest.Remote.Descriptors) > 0 {
			pw, _, _ := progress.FromContext(ctx)
			progressController := &controller.Controller{
				Writer: pw,
			}
			if p.vtx != nil {
				progressController.Digest = p.vtx.Digest()
				progressController.Name = p.vtx.Name()
			}

			descHandler := &cache.DescHandler{
				Provider: p.manifest.Remote.Provider,
				ImageRef: p.manifest.Ref,
				Progress: progressController,
			}

			p.descHandlers = cache.DescHandlers(make(map[digest.Digest]*cache.DescHandler))
			for _, desc := range p.manifest.Remote.Descriptors {
				p.descHandlers[desc.Digest] = descHandler
			}
		}

		desc := p.manifest.MainManifestDesc
		k, err := mainManifestKey(ctx, desc, p.Platform)
		if err != nil {
			p.cacheKeyErr = err
			return
		}
		p.manifestKey = k.String()

		dt, err := content.ReadBlob(ctx, p.ContentStore, p.manifest.ConfigDesc)
		if err != nil {
			p.cacheKeyErr = err
			return
		}
		p.configKey = cacheKeyFromConfig(dt).String()
	})
	if p.cacheKeyErr != nil {
		return "", nil, false, p.cacheKeyErr
	}

	cacheOpts = solver.CacheOpts(make(map[interface{}]interface{}))
	for dgst, descHandler := range p.descHandlers {
		cacheOpts[cache.DescHandlerKey(dgst)] = descHandler
	}

	cacheDone = index > 0
	if index == 0 || p.configKey == "" {
		return p.manifestKey, cacheOpts, cacheDone, nil
	}
	return p.configKey, cacheOpts, cacheDone, nil
}

func (p *puller) Snapshot(ctx context.Context, g session.Group) (ir cache.ImmutableRef, err error) {
	if p.Puller.Resolver == nil {
		p.Puller.Resolver = pull.NewResolver(g, p.ResolverOpt)
	} else {
		p.ResolverOpt.Auth.AddSession(g)
	}

	if len(p.manifest.Remote.Descriptors) == 0 {
		return nil, nil
	}
	defer p.releaseTmpLeases(ctx)

	var current cache.ImmutableRef
	defer func() {
		if err != nil && current != nil {
			current.Release(context.TODO())
		}
	}()

	var parent cache.ImmutableRef
	for _, layerDesc := range p.manifest.Remote.Descriptors {
		parent = current
		current, err = p.CacheAccessor.GetByBlob(ctx, layerDesc, parent, p.descHandlers)
		if parent != nil {
			parent.Release(context.TODO())
		}
		if err != nil {
			return nil, err
		}
	}

	for _, desc := range p.manifest.Nonlayers {
		if _, err := p.ContentStore.Info(ctx, desc.Digest); errdefs.IsNotFound(err) {
			// manifest or config must have gotten gc'd after CacheKey, re-pull them
			ctx, done, err := leaseutil.WithLease(ctx, p.LeaseManager, leaseutil.MakeTemporary)
			if err != nil {
				return nil, err
			}
			defer done(ctx)

			if _, err := p.PullManifests(ctx); err != nil {
				return nil, err
			}
		} else if err != nil {
			return nil, err
		}

		if err := p.LeaseManager.AddResource(ctx, leases.Lease{ID: current.ID()}, leases.Resource{
			ID:   desc.Digest.String(),
			Type: "content",
		}); err != nil {
			return nil, err
		}
	}

	if current != nil && p.Platform != nil && p.Platform.OS == "windows" && runtime.GOOS != "windows" {
		if err := markRefLayerTypeWindows(current); err != nil {
			return nil, err
		}
	}

	if p.id.RecordType != "" && cache.GetRecordType(current) == "" {
		if err := cache.SetRecordType(current, p.id.RecordType); err != nil {
			return nil, err
		}
	}

	return current, nil
}

func markRefLayerTypeWindows(ref cache.ImmutableRef) error {
	if parent := ref.Parent(); parent != nil {
		defer parent.Release(context.TODO())
		if err := markRefLayerTypeWindows(parent); err != nil {
			return err
		}
	}
	return cache.SetLayerType(ref, "windows")
}

// cacheKeyFromConfig returns a stable digest from image config. If image config
// is a known oci image we will use chainID of layers.
func cacheKeyFromConfig(dt []byte) digest.Digest {
	var img specs.Image
	err := json.Unmarshal(dt, &img)
	if err != nil {
		return digest.FromBytes(dt)
	}
	if img.RootFS.Type != "layers" || len(img.RootFS.DiffIDs) == 0 {
		return ""
	}
	return identity.ChainID(img.RootFS.DiffIDs)
}

func oneOffProgress(ctx context.Context, id string) func(err error) error {
	pw, _, _ := progress.FromContext(ctx)
	now := time.Now()
	st := progress.Status{
		Started: &now,
	}
	pw.Write(id, st)
	return func(err error) error {
		// TODO: set error on status
		now := time.Now()
		st.Completed = &now
		pw.Write(id, st)
		pw.Close()
		return err
	}
}
