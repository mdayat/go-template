// Package mobile serves the mobile apps, under /mobile/v1. Installs of old app versions keep
// calling these routes for years, so what they accept and reply only grows: add a field or a route;
// never remove, rename or retype one. A change that can't be made that way is a new version,
// /mobile/v2, served beside v1 until the request log's user agents show no old version calling
// (docs/standards/http-api.md). The rules live in the services the web routes share.
package mobile
