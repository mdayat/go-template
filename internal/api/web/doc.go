// Package web serves the web app, under /web. The web app is deployed with the API, so these routes
// change as it needs, without the mobile apps' rules; a field or route is renamed or removed in two
// releases (add the new one, then drop the old), as old and new copies overlap during a deploy
// (docs/standards/http-api.md). The rules live in the services the mobile routes share; only the
// HTTP side is the web app's own.
package web
