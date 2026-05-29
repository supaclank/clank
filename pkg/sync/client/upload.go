package syncclient

import "context"

// PutFile uploads the file at path to a presigned PUT URL using the
// client's HTTP transport — same semantics as the checkpoint bundle
// uploads. Exposed for callers that mint their own URLs (e.g. session
// blob uploads via sessionsync).
func (c *Client) PutFile(ctx context.Context, url, path string) error {
	return uploadFile(ctx, c.client, url, path)
}

// PutBytes uploads data to a presigned PUT URL with the given content type.
func (c *Client) PutBytes(ctx context.Context, url string, data []byte, contentType string) error {
	return uploadBytes(ctx, c.client, url, data, contentType)
}
