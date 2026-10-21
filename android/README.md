# Tasklane for Android

A native (Kotlin, Jetpack Compose, Material 3) read-mostly client for a Tasklane server. It talks to the server's own REST
API under `/api/v1`; nothing on the server had to change.

**What it does:** sign in (password, and an authenticator-app code when the account needs one), browse projects, search and
page through issues, open an issue (fields, description, comments), read a board column by column, read the notification
inbox, and add comments. **What it does not do yet:** edit fields, move issues, create issues, passkey sign-in, push.

It follows the device's light or dark setting (screenshots are in the [main README](../README.md#android)).

**Sign-in:** the server returns the session token as the `tl_session` cookie; the app keeps that value and sends it as
`Authorization: Bearer`. Sign out also sends it as the cookie, because that is the only form the server's logout revokes.
The token lives in app-private storage (`allowBackup` is off). The server address may be plain `http` for a LAN install.

## Build

Needs JDK 17+ and the Android SDK (platform 35, build-tools 35). Point `local.properties` at it (`sdk.dir=...`).

    ./gradlew :app:assembleDebug          # app/build/outputs/apk/debug/app-debug.apk
    ./gradlew :app:testDebugUnitTest      # 13 mock-server tests

`LiveServerTest` runs the real client against a real server and is skipped unless `TASKLANE_IT_URL`, `TASKLANE_IT_EMAIL`
and `TASKLANE_IT_PASSWORD` are set. Use a throwaway server: it needs a project `SHOP` with issue `SHOP-1` ("Checkout fails on 3DS challenge") and a board (the data the happy-path script uses), and
it adds a comment.

## Happy-path test on an emulator

`scripts/happy_path.py` drives the installed app over `adb`: sign in, search, open an issue, comment, projects, a board,
the inbox, sign out. It stops at the first screen that is not what it expects. See the header of the script for the data
the server needs; run it once per theme:

    sdkmanager "emulator" "system-images;android-35;google_apis;x86_64"
    avdmanager create avd -n pixel_api35 -k "system-images;android-35;google_apis;x86_64" -d pixel_6
    emulator -avd pixel_api35 &            # needs KVM to be usable
    adb install -r app/build/outputs/apk/debug/app-debug.apk
    python3 scripts/happy_path.py dark     # and: light

The emulator reaches your machine at `10.0.2.2`; the script signs in to `http://10.0.2.2:18091`.
