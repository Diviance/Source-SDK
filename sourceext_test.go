package sourceextsdk

import (
	"bytes"
	"context"
	"io"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"
	pb "onedev.diviance.club/MangaVault/Source-SDK/gen/sourceext/v1"
)

type testSource struct{ UnimplementedSourceExtension }

func (testSource) Manifest(context.Context) (*pb.ExtensionManifest, error) {
	return &pb.ExtensionManifest{ProtocolVersion: ProtocolVersion, Extension: &pb.ExtensionInfo{Id: "test", Version: "1.0.0"}}, nil
}

func (testSource) Image(context.Context, *pb.ImageRequest) (*Image, error) {
	data := bytes.Repeat([]byte("image"), MaxImageChunk/5+100)
	return &Image{Metadata: &pb.ImageMetadata{Mime: "image/test", ContentLength: int64(len(data))}, Body: io.NopCloser(bytes.NewReader(data))}, nil
}

func TestImageStreamingAndMetadata(t *testing.T) {
	listener := bufconn.Listen(2 << 20)
	server := grpc.NewServer()
	pb.RegisterSourceExtensionServer(server, &grpcServer{impl: testSource{}})
	go server.Serve(listener)
	t.Cleanup(server.Stop)

	conn, err := grpc.NewClient("passthrough:///bufnet", grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	client := &Client{raw: pb.NewSourceExtensionClient(conn), connectionContext: context.Background()}
	image, err := client.Image(context.Background(), &pb.ImageRequest{})
	if err != nil {
		t.Fatal(err)
	}
	defer image.Body.Close()
	if image.Metadata.Mime != "image/test" {
		t.Fatalf("unexpected mime %q", image.Metadata.Mime)
	}
	data, err := io.ReadAll(image.Body)
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(data)) != image.Metadata.ContentLength {
		t.Fatalf("read %d bytes, expected %d", len(data), image.Metadata.ContentLength)
	}
}

func TestStructuredErrorRoundTrip(t *testing.T) {
	original := &Error{GRPCCode: codes.ResourceExhausted, Code: "rate_limited", Message: "slow down", SourceID: "source", Retryable: true, RetryAfter: 3 * time.Second, UpstreamStatus: 429}
	parsed := ParseError(original.GRPCStatus().Err())
	if parsed.Code != original.Code || parsed.SourceID != original.SourceID || parsed.RetryAfter != original.RetryAfter || parsed.UpstreamStatus != 429 {
		t.Fatalf("unexpected parsed error: %#v", parsed)
	}
}

func TestOptionalFieldsAndTypedFilterChangesRoundTrip(t *testing.T) {
	empty := ""
	original := &pb.SearchRequest{
		SourceId: "source",
		Filters: []*pb.FilterChange{{
			Position: 2,
			Path:     []int32{1, 3},
			State:    &pb.FilterChange_Sort{Sort: &pb.SortSelection{Index: 4, Ascending: true}},
		}},
	}
	manga := &pb.SManga{Url: "/manga", Title: "Title", Artist: &empty}
	requestBytes, err := proto.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	var decoded pb.SearchRequest
	if err := proto.Unmarshal(requestBytes, &decoded); err != nil {
		t.Fatal(err)
	}
	selection := decoded.Filters[0].GetSort()
	if selection == nil || selection.Index != 4 || !selection.Ascending || len(decoded.Filters[0].Path) != 2 {
		t.Fatalf("typed filter state was not preserved: %#v", decoded.Filters[0])
	}
	mangaBytes, err := proto.Marshal(manga)
	if err != nil {
		t.Fatal(err)
	}
	var decodedManga pb.SManga
	if err := proto.Unmarshal(mangaBytes, &decodedManga); err != nil {
		t.Fatal(err)
	}
	if decodedManga.Artist == nil || *decodedManga.Artist != "" || decodedManga.Author != nil {
		t.Fatalf("optional presence was not preserved: %v", &decodedManga)
	}
}
