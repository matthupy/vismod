package google

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	vision "cloud.google.com/go/vision/v2/apiv1"
	pb "cloud.google.com/go/vision/v2/apiv1/visionpb"
	"google.golang.org/api/option"
	spb "google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"github.com/vismod/vismod/internal/moderate"
	"github.com/vismod/vismod/pkg/moderation"
)

// fakeVision is a local, in-process stand-in for the Cloud Vision gRPC
// service. It exists to prove the REQUEST vismod sends (the part goldens
// cannot check, since goldens are built from responses this repo authored)
// and the response handling in sdkAnnotator.
type fakeVision struct {
	pb.UnimplementedImageAnnotatorServer

	got  *pb.BatchAnnotateImagesRequest
	resp *pb.BatchAnnotateImagesResponse
	err  error
}

func (f *fakeVision) BatchAnnotateImages(_ context.Context, req *pb.BatchAnnotateImagesRequest) (*pb.BatchAnnotateImagesResponse, error) {
	f.got = req
	if f.err != nil {
		return nil, f.err
	}
	return f.resp, nil
}

// newFakeClient wires a real SDK client to the fake service over an
// in-memory listener: no network, no credentials, real protobuf encoding.
func newFakeClient(t *testing.T, svc *fakeVision) annotator {
	t.Helper()
	lis := bufconn.Listen(1024 * 1024)
	srv := grpc.NewServer()
	pb.RegisterImageAnnotatorServer(srv, svc)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial fake vision: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	client, err := vision.NewImageAnnotatorClient(context.Background(), option.WithGRPCConn(conn))
	if err != nil {
		t.Fatalf("vision client: %v", err)
	}
	return sdkAnnotator{client}
}

// TestRequestAsksForSafeSearchOnly: vismod must request exactly the
// SafeSearch feature. Asking for more would bill for annotations it never
// reads (and pull back OCR/labels, which invariant 3 keeps out entirely).
func TestRequestAsksForSafeSearchOnly(t *testing.T) {
	svc := &fakeVision{resp: &pb.BatchAnnotateImagesResponse{
		Responses: []*pb.AnnotateImageResponse{{
			SafeSearchAnnotation: &pb.SafeSearchAnnotation{Adult: pb.Likelihood_VERY_LIKELY},
		}},
	}}
	m := newWith(newFakeClient(t, svc), options{})

	res, err := m.AnalyzeImage(context.Background(), moderation.Image{Bytes: []byte("image-bytes"), MIME: "image/png"})
	if err != nil {
		t.Fatalf("AnalyzeImage: %v", err)
	}

	if svc.got == nil || len(svc.got.GetRequests()) != 1 {
		t.Fatalf("fake service saw %+v, want exactly one annotate request", svc.got)
	}
	req := svc.got.GetRequests()[0]
	feats := req.GetFeatures()
	if len(feats) != 1 || feats[0].GetType() != pb.Feature_SAFE_SEARCH_DETECTION {
		t.Errorf("features = %+v, want only SAFE_SEARCH_DETECTION", feats)
	}
	if string(req.GetImage().GetContent()) != "image-bytes" {
		t.Errorf("image content = %q, want the frame bytes inline", req.GetImage().GetContent())
	}
	if req.GetImage().GetSource() != nil {
		t.Error("request carries an image SOURCE; vismod must upload bytes, never hand the vendor a URI to fetch")
	}

	// And the response still normalizes: VERY_LIKELY adult is a 1.0 SEXUAL.
	if len(res.Frames) != 1 {
		t.Fatalf("want one frame result, got %+v", res.Frames)
	}
	var found bool
	for _, c := range res.Frames[0].Categories {
		if c.Category == moderation.CategorySexual {
			found = true
			if c.Score == nil || *c.Score != 1.0 {
				t.Errorf("SEXUAL score = %v, want 1.0 for VERY_LIKELY", c.Score)
			}
		}
	}
	if !found {
		t.Error("no SEXUAL category in the normalized result")
	}
}

// TestTransportFailureIsRetryableNeverAllow: a gRPC failure that survives
// the SDK's own retries must reach the pipeline as retryable, so the job
// ends at verdict=error rather than being scored on nothing.
//
// The injected code is InvalidArgument, not Unavailable: the SDK retries
// Unavailable internally with its own backoff, so a permanently-failing
// fake would keep the call in the SDK's retry loop rather than returning.
func TestTransportFailureIsRetryableNeverAllow(t *testing.T) {
	svc := &fakeVision{err: status.Error(codes.InvalidArgument, "malformed request")}
	m := newWith(newFakeClient(t, svc), options{})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := m.AnalyzeImage(ctx, moderation.Image{Bytes: []byte("x")})
	if err == nil {
		t.Fatal("a transport failure was reported as a successful analysis")
	}
	if !moderation.IsRetryable(err) {
		t.Errorf("error %v is not retryable; a transient outage would dead-letter immediately", err)
	}
}

// TestPerResponseErrorIsSurfaced: the batch call can succeed at the
// transport level while the single response carries an error. Reading the
// (empty) annotation instead would score the frame as all-UNKNOWN.
func TestPerResponseErrorIsSurfaced(t *testing.T) {
	svc := &fakeVision{resp: &pb.BatchAnnotateImagesResponse{
		Responses: []*pb.AnnotateImageResponse{{
			Error: &spb.Status{Code: 3, Message: "bad image data"},
		}},
	}}
	m := newWith(newFakeClient(t, svc), options{})

	if _, err := m.AnalyzeImage(context.Background(), moderation.Image{Bytes: []byte("x")}); err == nil {
		t.Fatal("a per-response error was treated as a successful annotation")
	}
}

// TestEmptyBatchResponseIsAnError: zero responses is could-not-evaluate,
// never a clean result.
func TestEmptyBatchResponseIsAnError(t *testing.T) {
	svc := &fakeVision{resp: &pb.BatchAnnotateImagesResponse{}}
	m := newWith(newFakeClient(t, svc), options{})

	if _, err := m.AnalyzeImage(context.Background(), moderation.Image{Bytes: []byte("x")}); err == nil {
		t.Fatal("an empty batch response was treated as a successful annotation")
	}
}

// TestCloseReleasesTheClient: serve holds one Moderator for the process
// lifetime and closes it on drain; a Close that does not reach the SDK
// leaks the gRPC connection.
func TestCloseReleasesTheClient(t *testing.T) {
	m := newWith(newFakeClient(t, &fakeVision{resp: &pb.BatchAnnotateImagesResponse{}}), options{})
	if err := m.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

// TestNewRejectsMalformedOptionsBeforeTouchingCredentials: an options typo
// must fail with a message about the option, not about missing ADC — the
// order of the two checks decides which error an operator sees.
func TestNewRejectsMalformedOptionsBeforeTouchingCredentials(t *testing.T) {
	_, err := New(moderate.AdapterConfig{Options: map[string]any{"rate_limit_rps": "fifteen"}})
	if err == nil {
		t.Fatal("a non-numeric rate_limit_rps was accepted")
	}
	if got := err.Error(); !strings.Contains(got, "options") {
		t.Errorf("error = %q, want it to name the bad option rather than credentials", got)
	}
}
