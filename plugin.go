package sourceextsdk

import (
	"context"
	"io"
	"os"

	pb "gitea.diviance.club/Diviance/Source-SDK/Source-SDK/gen/sourceext/v1"
	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/go-plugin"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/emptypb"
)

var Handshake = plugin.HandshakeConfig{
	MagicCookieKey:   "MANGAVAULT_SOURCE_EXTENSION",
	MagicCookieValue: "mangavault-source-extension-v1",
}

func VersionedPluginSet(impl SourceExtension) map[int]plugin.PluginSet {
	return map[int]plugin.PluginSet{
		ProtocolVersion: {
			PluginName: &GRPCPlugin{Impl: impl},
		},
	}
}

type GRPCPlugin struct {
	plugin.NetRPCUnsupportedPlugin
	Impl SourceExtension
}

func (p *GRPCPlugin) GRPCServer(_ *plugin.GRPCBroker, server *grpc.Server) error {
	pb.RegisterSourceExtensionServer(server, &grpcServer{impl: p.Impl})
	return nil
}

func (*GRPCPlugin) GRPCClient(ctx context.Context, _ *plugin.GRPCBroker, conn *grpc.ClientConn) (interface{}, error) {
	return &Client{raw: pb.NewSourceExtensionClient(conn), connectionContext: ctx}, nil
}

type ServeOptions struct {
	Logger hclog.Logger
}

// Serve starts the extension worker and does not return.
func Serve(impl SourceExtension, options ServeOptions) {
	logger := options.Logger
	if logger == nil {
		logger = hclog.New(&hclog.LoggerOptions{
			Name:       "source-extension",
			Level:      hclog.Info,
			Output:     os.Stderr,
			JSONFormat: true,
		})
	}
	plugin.Serve(&plugin.ServeConfig{
		HandshakeConfig:  Handshake,
		VersionedPlugins: VersionedPluginSet(impl),
		GRPCServer:       plugin.DefaultGRPCServer,
		Logger:           logger,
	})
}

type grpcServer struct {
	pb.UnimplementedSourceExtensionServer
	impl SourceExtension
}

func (s *grpcServer) GetManifest(ctx context.Context, _ *emptypb.Empty) (*pb.ExtensionManifest, error) {
	return s.impl.Manifest(ctx)
}

func (s *grpcServer) PopularManga(ctx context.Context, req *pb.ListingRequest) (*pb.MangasPage, error) {
	return s.impl.PopularManga(ctx, req)
}

func (s *grpcServer) LatestUpdates(ctx context.Context, req *pb.ListingRequest) (*pb.MangasPage, error) {
	return s.impl.LatestUpdates(ctx, req)
}

func (s *grpcServer) SearchManga(ctx context.Context, req *pb.SearchRequest) (*pb.MangasPage, error) {
	return s.impl.SearchManga(ctx, req)
}

func (s *grpcServer) MangaUpdate(ctx context.Context, req *pb.MangaUpdateRequest) (*pb.SMangaUpdate, error) {
	return s.impl.MangaUpdate(ctx, req)
}

func (s *grpcServer) PageList(ctx context.Context, req *pb.PageListRequest) (*pb.PageListResponse, error) {
	return s.impl.PageList(ctx, req)
}

func (s *grpcServer) ResolveImageURL(ctx context.Context, req *pb.PageURLRequest) (*pb.URLResponse, error) {
	return s.impl.ResolveImageURL(ctx, req)
}

func (s *grpcServer) MangaURL(ctx context.Context, req *pb.MangaURLRequest) (*pb.URLResponse, error) {
	return s.impl.MangaURL(ctx, req)
}

func (s *grpcServer) ChapterURL(ctx context.Context, req *pb.ChapterURLRequest) (*pb.URLResponse, error) {
	return s.impl.ChapterURL(ctx, req)
}

func (s *grpcServer) GetImage(req *pb.ImageRequest, stream pb.SourceExtension_GetImageServer) error {
	image, err := s.impl.Image(stream.Context(), req)
	if err != nil {
		return err
	}
	if image == nil || image.Body == nil {
		return &Error{Code: "empty_image", Message: "source returned an empty image"}
	}
	defer image.Body.Close()
	metadata := image.Metadata
	if metadata == nil {
		metadata = &pb.ImageMetadata{}
	}
	if err := stream.Send(&pb.ImageStreamFrame{Payload: &pb.ImageStreamFrame_Metadata{Metadata: metadata}}); err != nil {
		return err
	}
	buffer := make([]byte, MaxImageChunk)
	for {
		n, readErr := image.Body.Read(buffer)
		if n > 0 {
			data := append([]byte(nil), buffer[:n]...)
			if err := stream.Send(&pb.ImageStreamFrame{Payload: &pb.ImageStreamFrame_Data{Data: data}}); err != nil {
				return err
			}
		}
		if readErr == io.EOF {
			return nil
		}
		if readErr != nil {
			return readErr
		}
	}
}
