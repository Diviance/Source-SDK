package sourceextsdk

import (
	"context"
	"io"

	pb "gitea.diviance.club/Diviance/Source-SDK/gen/sourceext/v1"
)

const (
	ProtocolVersion = 1
	PluginName      = "source_extension"
	MaxImageChunk   = 256 * 1024
)

// SourceExtension is the implementation contract for a generated source
// extension. The SDK translates it to and from the versioned gRPC service.
type SourceExtension interface {
	Manifest(context.Context) (*pb.ExtensionManifest, error)
	PopularManga(context.Context, *pb.ListingRequest) (*pb.MangasPage, error)
	LatestUpdates(context.Context, *pb.ListingRequest) (*pb.MangasPage, error)
	SearchManga(context.Context, *pb.SearchRequest) (*pb.MangasPage, error)
	MangaUpdate(context.Context, *pb.MangaUpdateRequest) (*pb.SMangaUpdate, error)
	PageList(context.Context, *pb.PageListRequest) (*pb.PageListResponse, error)
	ResolveImageURL(context.Context, *pb.PageURLRequest) (*pb.URLResponse, error)
	MangaURL(context.Context, *pb.MangaURLRequest) (*pb.URLResponse, error)
	ChapterURL(context.Context, *pb.ChapterURLRequest) (*pb.URLResponse, error)
	Image(context.Context, *pb.ImageRequest) (*Image, error)
}

// Image is streamed by the SDK without buffering the full response in memory.
type Image struct {
	Metadata *pb.ImageMetadata
	Body     io.ReadCloser
}

// UnimplementedSourceExtension provides forward-compatible defaults.
type UnimplementedSourceExtension struct{}

func (UnimplementedSourceExtension) Manifest(context.Context) (*pb.ExtensionManifest, error) {
	return nil, Unimplemented("manifest")
}
func (UnimplementedSourceExtension) PopularManga(context.Context, *pb.ListingRequest) (*pb.MangasPage, error) {
	return nil, Unimplemented("popular manga")
}
func (UnimplementedSourceExtension) LatestUpdates(context.Context, *pb.ListingRequest) (*pb.MangasPage, error) {
	return nil, Unimplemented("latest updates")
}
func (UnimplementedSourceExtension) SearchManga(context.Context, *pb.SearchRequest) (*pb.MangasPage, error) {
	return nil, Unimplemented("search manga")
}
func (UnimplementedSourceExtension) MangaUpdate(context.Context, *pb.MangaUpdateRequest) (*pb.SMangaUpdate, error) {
	return nil, Unimplemented("manga update")
}
func (UnimplementedSourceExtension) PageList(context.Context, *pb.PageListRequest) (*pb.PageListResponse, error) {
	return nil, Unimplemented("page list")
}
func (UnimplementedSourceExtension) ResolveImageURL(context.Context, *pb.PageURLRequest) (*pb.URLResponse, error) {
	return nil, Unimplemented("image URL")
}
func (UnimplementedSourceExtension) MangaURL(context.Context, *pb.MangaURLRequest) (*pb.URLResponse, error) {
	return nil, Unimplemented("manga URL")
}
func (UnimplementedSourceExtension) ChapterURL(context.Context, *pb.ChapterURLRequest) (*pb.URLResponse, error) {
	return nil, Unimplemented("chapter URL")
}
func (UnimplementedSourceExtension) Image(context.Context, *pb.ImageRequest) (*Image, error) {
	return nil, Unimplemented("image")
}
