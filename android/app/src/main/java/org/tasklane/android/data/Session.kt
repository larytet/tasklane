package org.tasklane.android.data

import android.content.Context

/** What survives a restart: the server, the token and who is signed in. App-private storage; nothing is backed up. */
class Session(context: Context) {
    private val prefs = context.getSharedPreferences("session", Context.MODE_PRIVATE)

    var server: String
        get() = prefs.getString("server", "") ?: ""
        set(v) = prefs.edit().putString("server", v).apply()
    var token: String?
        get() = prefs.getString("token", null)
        set(v) = prefs.edit().putString("token", v).apply()
    var userName: String
        get() = prefs.getString("name", "") ?: ""
        set(v) = prefs.edit().putString("name", v).apply()

    val signedIn get() = server.isNotEmpty() && token != null

    fun signOut() = prefs.edit().remove("token").remove("name").apply()
}
