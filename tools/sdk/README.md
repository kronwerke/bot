# Discord Embedded App SDK, bundled

The whitelist app loads Discord's SDK from its own address, because an Activity may not
load scripts from anywhere else. `internal/wlweb/web/sdk.js` is built here:

    npm install @discord/embedded-app-sdk@2.5.0 esbuild
    npx esbuild entry.js --bundle --minify --format=esm --target=es2020 --outfile=../../internal/wlweb/web/sdk.js

The SDK is MIT licensed, Copyright (c) 2024 Discord Inc.
