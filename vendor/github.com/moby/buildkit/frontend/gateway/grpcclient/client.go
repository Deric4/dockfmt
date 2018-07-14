package grpcclient

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"strings"
	"time"

	"github.com/gogo/googleapis/google/rpc"
	"github.com/moby/buildkit/frontend/gateway/client"
	pb "github.com/moby/buildkit/frontend/gateway/pb"
	opspb "github.com/moby/buildkit/solver/pb"
	"github.com/moby/buildkit/util/apicaps"
	digest "github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
	"google.golang.org/grpc"
	"google.golang.org/grpc/status"
)

const frontendPrefix = "BUILDKIT_FRONTEND_OPT_"

func current() (*grpcClient, error) {
	if ep := product(); ep != "" {
		apicaps.ExportedProduct = ep
	}

	ctx, conn, err := grpcClientConn(context.Background())
	if err != nil {
		return nil, err
	}

	c := pb.NewLLBBridgeClient(conn)

	resp, err := c.Ping(ctx, &pb.PingRequest{})
	if err != nil {
		return nil, err
	}

	if resp.FrontendAPICaps == nil {
		resp.FrontendAPICaps = defaultCaps()
	}

	return &grpcClient{
		client:    c,
		opts:      opts(),
		sessionID: sessionID(),
		workers:   workers(),
		product:   product(),
		caps:      pb.Caps.CapSet(resp.FrontendAPICaps),
		requests:  map[string]*pb.SolveRequest{},
	}, nil
}

func convertRef(ref client.Reference) (string, error) {
	if ref == nil {
		return "", nil
	}
	r, ok := ref.(*reference)
	if !ok {
		return "", errors.Errorf("invalid return reference type %T", ref)
	}
	return r.id, nil
}

func Run(ctx context.Context, f client.BuildFunc) (retError error) {
	client, err := current()
	if err != nil {
		return errors.Wrapf(err, "failed to initialize client from environment")
	}

	res, err := f(ctx, client)

	export := client.caps.Supports(pb.CapReturnResult) == nil

	if export {
		defer func() {
			req := &pb.ReturnRequest{}
			if retError == nil {
				pbRes := &pb.Result{
					Metadata: res.Metadata,
				}
				if res.Refs != nil {
					m := map[string]string{}
					for k, r := range res.Refs {
						id, err := convertRef(r)
						if err != nil {
							retError = err
							continue
						}
						m[k] = id
					}
					pbRes.Result = &pb.Result_Refs{Refs: &pb.RefMap{Refs: m}}
				} else {
					id, err := convertRef(res.Ref)
					if err != nil {
						retError = err
					} else {
						pbRes.Result = &pb.Result_Ref{Ref: id}
					}
				}
				if retError == nil {
					req.Result = pbRes
				}
			}
			if retError != nil {
				st, _ := status.FromError(retError)
				stp := st.Proto()
				req.Error = &rpc.Status{
					Code:    stp.Code,
					Message: stp.Message,
					// Details: stp.Details,
				}
			}
			if _, err := client.client.Return(ctx, req); err != nil && retError == nil {
				retError = err
			}
		}()
	}

	if err != nil {
		return err
	}

	if err := client.caps.Supports(pb.CapReturnMap); len(res.Refs) > 1 && err != nil {
		return err
	}

	if !export {
		exportedAttrBytes, err := json.Marshal(res.Metadata)
		if err != nil {
			return errors.Wrapf(err, "failed to marshal return metadata")
		}

		req, err := client.requestForRef(res.Ref)
		if err != nil {
			return errors.Wrapf(err, "failed to find return ref")
		}

		req.Final = true
		req.ExporterAttr = exportedAttrBytes

		if _, err := client.client.Solve(ctx, req); err != nil {
			return errors.Wrapf(err, "failed to solve")
		}
	}

	return nil
}

// defaultCaps returns the capabilities that were implemented when capabilities
// support was added. This list is frozen and should never be changed.
func defaultCaps() []apicaps.PBCap {
	return []apicaps.PBCap{
		{ID: string(pb.CapSolveBase), Enabled: true},
		{ID: string(pb.CapSolveInlineReturn), Enabled: true},
		{ID: string(pb.CapResolveImage), Enabled: true},
		{ID: string(pb.CapReadFile), Enabled: true},
	}
}

type grpcClient struct {
	client    pb.LLBBridgeClient
	opts      map[string]string
	sessionID string
	product   string
	workers   []client.WorkerInfo
	caps      apicaps.CapSet
	requests  map[string]*pb.SolveRequest
}

func (c *grpcClient) requestForRef(ref client.Reference) (*pb.SolveRequest, error) {
	emptyReq := &pb.SolveRequest{
		Definition: &opspb.Definition{},
	}
	if ref == nil {
		return emptyReq, nil
	}
	r, ok := ref.(*reference)
	if !ok {
		return nil, errors.Errorf("return reference has invalid type %T", ref)
	}
	if r.id == "" {
		return emptyReq, nil
	}
	req, ok := c.requests[r.id]
	if !ok {
		return nil, errors.Errorf("did not find request for return reference %s", r.id)
	}
	return req, nil
}

func (c *grpcClient) Solve(ctx context.Context, creq client.SolveRequest) (*client.Result, error) {
	req := &pb.SolveRequest{
		Definition:        creq.Definition,
		Frontend:          creq.Frontend,
		FrontendOpt:       creq.FrontendOpt,
		ImportCacheRefs:   creq.ImportCacheRefs,
		AllowResultReturn: true,
	}

	// backwards compatibility with inline return
	if c.caps.Supports(pb.CapReturnResult) != nil {
		req.ExporterAttr = []byte("{}")
	}

	resp, err := c.client.Solve(ctx, req)
	if err != nil {
		return nil, err
	}

	res := &client.Result{}

	if resp.Result == nil {
		if id := resp.Ref; id != "" {
			c.requests[id] = req
		}
		res.SetRef(&reference{id: resp.Ref, c: c})
	} else {
		res.Metadata = resp.Result.Metadata
		switch pbRes := resp.Result.Result.(type) {
		case *pb.Result_Ref:
			if id := pbRes.Ref; id != "" {
				res.SetRef(&reference{id: id, c: c})
			}
		case *pb.Result_Refs:
			for k, v := range pbRes.Refs.Refs {
				ref := &reference{id: v, c: c}
				if v == "" {
					ref = nil
				}
				res.AddRef(k, ref)
			}
		}
	}

	return res, nil
}

func (c *grpcClient) ResolveImageConfig(ctx context.Context, ref string, platform *specs.Platform) (digest.Digest, []byte, error) {
	var p *opspb.Platform
	if platform != nil {
		p = &opspb.Platform{
			OS:           platform.OS,
			Architecture: platform.Architecture,
			Variant:      platform.Variant,
			OSVersion:    platform.OSVersion,
			OSFeatures:   platform.OSFeatures,
		}
	}
	resp, err := c.client.ResolveImageConfig(ctx, &pb.ResolveImageConfigRequest{Ref: ref, Platform: p})
	if err != nil {
		return "", nil, err
	}
	return resp.Digest, resp.Config, nil
}

func (c *grpcClient) BuildOpts() client.BuildOpts {
	return client.BuildOpts{
		Opts:      c.opts,
		SessionID: c.sessionID,
		Workers:   c.workers,
		Product:   c.product,
	}
}

func (c *grpcClient) Opts() map[string]string {
	return c.opts
}

func (c *grpcClient) SessionID() string {
	return c.sessionID
}

func (c *grpcClient) WorkerInfos() []client.WorkerInfo {
	return c.workers
}

type reference struct {
	id string
	c  *grpcClient
}

func (r *reference) ReadFile(ctx context.Context, req client.ReadRequest) ([]byte, error) {
	rfr := &pb.ReadFileRequest{FilePath: req.Filename, Ref: r.id}
	if r := req.Range; r != nil {
		rfr.Range = &pb.FileRange{
			Offset: int64(r.Offset),
			Length: int64(r.Length),
		}
	}
	resp, err := r.c.client.ReadFile(ctx, rfr)
	if err != nil {
		return nil, err
	}
	return resp.Data, nil
}

func grpcClientConn(ctx context.Context) (context.Context, *grpc.ClientConn, error) {
	dialOpt := grpc.WithDialer(func(addr string, d time.Duration) (net.Conn, error) {
		return stdioConn(), nil
	})

	cc, err := grpc.DialContext(ctx, "", dialOpt, grpc.WithInsecure())
	if err != nil {
		return nil, nil, errors.Wrap(err, "failed to create grpc client")
	}

	ctx, cancel := context.WithCancel(ctx)
	_ = cancel
	// go monitorHealth(ctx, cc, cancel)

	return ctx, cc, nil
}

func stdioConn() net.Conn {
	return &conn{os.Stdin, os.Stdout, os.Stdout}
}

type conn struct {
	io.Reader
	io.Writer
	io.Closer
}

func (s *conn) LocalAddr() net.Addr {
	return dummyAddr{}
}
func (s *conn) RemoteAddr() net.Addr {
	return dummyAddr{}
}
func (s *conn) SetDeadline(t time.Time) error {
	return nil
}
func (s *conn) SetReadDeadline(t time.Time) error {
	return nil
}
func (s *conn) SetWriteDeadline(t time.Time) error {
	return nil
}

type dummyAddr struct {
}

func (d dummyAddr) Network() string {
	return "pipe"
}

func (d dummyAddr) String() string {
	return "localhost"
}

func opts() map[string]string {
	opts := map[string]string{}
	for _, env := range os.Environ() {
		parts := strings.SplitN(env, "=", 2)
		k := parts[0]
		v := ""
		if len(parts) == 2 {
			v = parts[1]
		}
		if !strings.HasPrefix(k, frontendPrefix) {
			continue
		}
		parts = strings.SplitN(v, "=", 2)
		v = ""
		if len(parts) == 2 {
			v = parts[1]
		}
		opts[parts[0]] = v
	}
	return opts
}

func sessionID() string {
	return os.Getenv("BUILDKIT_SESSION_ID")
}

func workers() []client.WorkerInfo {
	var c []client.WorkerInfo
	if err := json.Unmarshal([]byte(os.Getenv("BUILDKIT_WORKERS")), &c); err != nil {
		return nil
	}
	return c
}

func product() string {
	return os.Getenv("BUILDKIT_EXPORTEDPRODUCT")
}
