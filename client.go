package sourceextsdk

import (
	"context"
	"fmt"
	"io"
	"sync"

	"google.golang.org/protobuf/types/known/emptypb"
	pb "mangavault/sourceextsdk/gen/sourceext/v1"
)

type Client struct {
	raw               pb.SourceExtensionClient
	connectionContext context.Context
}

func (c *Client) Manifest(ctx context.Context) (*pb.ExtensionManifest, error) {
	return c.raw.GetManifest(ctx, &emptypb.Empty{})
}
func (c *Client) PopularManga(ctx context.Context, req *pb.ListingRequest) (*pb.MangasPage, error) {
	return c.raw.PopularManga(ctx, req)
}
func (c *Client) LatestUpdates(ctx context.Context, req *pb.ListingRequest) (*pb.MangasPage, error) {
	return c.raw.LatestUpdates(ctx, req)
}
func (c *Client) SearchManga(ctx context.Context, req *pb.SearchRequest) (*pb.MangasPage, error) {
	return c.raw.SearchManga(ctx, req)
}
func (c *Client) MangaUpdate(ctx context.Context, req *pb.MangaUpdateRequest) (*pb.SMangaUpdate, error) {
	return c.raw.MangaUpdate(ctx, req)
}
func (c *Client) PageList(ctx context.Context, req *pb.PageListRequest) (*pb.PageListResponse, error) {
	return c.raw.PageList(ctx, req)
}
func (c *Client) ResolveImageURL(ctx context.Context, req *pb.PageURLRequest) (*pb.URLResponse, error) {
	return c.raw.ResolveImageURL(ctx, req)
}
func (c *Client) MangaURL(ctx context.Context, req *pb.MangaURLRequest) (*pb.URLResponse, error) {
	return c.raw.MangaURL(ctx, req)
}
func (c *Client) ChapterURL(ctx context.Context, req *pb.ChapterURLRequest) (*pb.URLResponse, error) {
	return c.raw.ChapterURL(ctx, req)
}

func (c *Client) Image(ctx context.Context, req *pb.ImageRequest) (*Image, error) {
	streamCtx, cancel := context.WithCancel(ctx)
	stream, err := c.raw.GetImage(streamCtx, req)
	if err != nil {
		cancel()
		return nil, err
	}
	first, err := stream.Recv()
	if err != nil {
		cancel()
		return nil, err
	}
	metadata := first.GetMetadata()
	if metadata == nil {
		cancel()
		return nil, fmt.Errorf("image stream did not start with metadata")
	}
	return &Image{
		Metadata: metadata,
		Body: &imageReader{
			stream: stream,
			cancel: cancel,
		},
	}, nil
}

type imageReader struct {
	stream pb.SourceExtension_GetImageClient
	cancel context.CancelFunc
	mu     sync.Mutex
	data   []byte
	closed bool
}

func (r *imageReader) Read(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return 0, io.ErrClosedPipe
	}
	for len(r.data) == 0 {
		frame, err := r.stream.Recv()
		if err != nil {
			return 0, err
		}
		if frame.GetMetadata() != nil {
			return 0, fmt.Errorf("image stream contained duplicate metadata")
		}
		r.data = frame.GetData()
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, nil
}

func (r *imageReader) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.closed {
		r.closed = true
		r.cancel()
	}
	return nil
}
