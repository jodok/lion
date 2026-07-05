package voyager

import (
	"context"
	"net/url"
	"strings"
)

// LinkedIn's modern Voyager surface is GraphQL. Requests are GET:
//
//	/voyager/api/graphql?includeWebMetadata=true&variables=(...)&queryId=<id>
//
// Two things make this awkward and are handled here:
//
//  1. The `variables` value uses LinkedIn's Rest.li encoding, NOT standard form
//     encoding: parentheses, commas and colons are structural and must stay
//     literal; only individual values (e.g. free-text keywords) are percent-
//     encoded. So we assemble the query string by hand instead of url.Values.
//
//  2. queryId hashes (e.g. voyagerSearchDashClusters.a7a0567f…) are pinned to a
//     specific LinkedIn web-app build and ROTATE over time. When a GraphQL call
//     starts returning 400/"missing query", refresh the constants below from a
//     logged-in browser's network tab. They are centralized here on purpose so
//     that refresh is a one-file change. (See DESIGN.md §"queryId maintenance".)
const (
	// queryIDProfile fetches a member profile by identity. Verified 2026-07-06.
	queryIDProfile = "voyagerIdentityDashProfiles.b5c27c04968c409fc0ed3546575b9b7a"
	// queryIDSearchClusters runs blended/entity search. Verified 2026-07-06.
	queryIDSearchClusters = "voyagerSearchDashClusters.a7a0567fa66c52d645b5ff2f960b92aa"
)

// getGraphQL issues a Voyager GraphQL GET. variables is the pre-assembled
// Rest.li-encoded variables string (without the surrounding "variables=").
func (c *Client) getGraphQL(ctx context.Context, queryID, variables string) ([]byte, error) {
	// Build the raw query string ourselves so Rest.li structure survives.
	raw := "includeWebMetadata=true&variables=" + variables + "&queryId=" + queryID
	return c.getRawQuery(ctx, "/graphql", raw)
}

// rlEncode percent-encodes a value for use inside a Rest.li variables string,
// escaping the characters that would otherwise be read as structure (parens,
// commas, colons). Spaces become %20 (not "+"): LinkedIn's Rest.li layer reads
// "+" literally, so form-encoding would corrupt multi-word keywords.
func rlEncode(s string) string {
	return strings.ReplaceAll(url.QueryEscape(s), "+", "%20")
}
