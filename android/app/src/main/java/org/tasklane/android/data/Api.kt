package org.tasklane.android.data

import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext
import kotlinx.serialization.json.Json
import kotlinx.serialization.json.buildJsonObject
import kotlinx.serialization.json.put
import okhttp3.HttpUrl
import okhttp3.HttpUrl.Companion.toHttpUrl
import okhttp3.MediaType.Companion.toMediaType
import okhttp3.OkHttpClient
import okhttp3.Request
import okhttp3.RequestBody.Companion.toRequestBody
import okhttp3.Response
import java.io.IOException
import java.util.concurrent.TimeUnit

class ApiException(val status: Int, val code: String, message: String) : IOException(message) {
    val unauthorized get() = status == 401
}

sealed interface LoginResult {
    data class SignedIn(val token: String, val user: User) : LoginResult
    data class NeedsCode(val mfaToken: String) : LoginResult
}

val apiJson = Json { ignoreUnknownKeys = true; coerceInputValues = true; explicitNulls = false }

private const val COOKIE = "tl_session"
private val JSON_TYPE = "application/json".toMediaType()

/**
 * The Tasklane REST API. Sign-in answers with the session token only as a `tl_session` cookie; the same value is accepted
 * as `Authorization: Bearer`, so the app reads it from the response and sends it that way (no cookie jar, no CSRF header).
 */
class TasklaneApi(
    serverUrl: String,
    private val token: () -> String?,
    private val http: OkHttpClient = defaultClient,
) {
    private val base: HttpUrl = normalizeServerUrl(serverUrl).toHttpUrl()

    suspend fun login(email: String, password: String): LoginResult {
        val body = buildJsonObject { put("email", email); put("password", password) }.toString()
        return finishLogin(call(post("login", body), auth = false))
    }

    suspend fun loginMfa(mfaToken: String, code: String): LoginResult {
        val body = buildJsonObject { put("mfa_token", mfaToken); put("code", code) }.toString()
        return finishLogin(call(post("login/mfa", body), auth = false))
    }

    private fun finishLogin(r: Raw): LoginResult {
        val reply = apiJson.decodeFromString(LoginReply.serializer(), r.body)
        if (reply.mfaRequired) {
            if (!reply.totp) throw ApiException(400, "unsupported", "This account signs in with a passkey; use an authenticator-app code or sign in on the web first.")
            return LoginResult.NeedsCode(reply.mfaToken)
        }
        val tok = r.cookie ?: throw ApiException(500, "no_session", "The server did not return a session")
        return LoginResult.SignedIn(tok, User(reply.id, reply.email, reply.name))
    }

    /**
     * The server revokes the session named by the cookie only, so the token is also sent as the cookie (with the header
     * that cookie-authenticated writes require); a Bearer-only request would answer 200 and leave the session alive.
     */
    suspend fun logout() {
        val tok = token() ?: return
        runCatching { call(post("logout", "{}").header("Cookie", "$COOKIE=$tok").header("X-Requested-With", "tasklane")) }
    }

    suspend fun me(): User = get("me", User.serializer())

    suspend fun projects(): List<Project> = get("projects", kotlinx.serialization.builtins.ListSerializer(Project.serializer()))

    suspend fun issues(query: String, text: Boolean, offset: Int, limit: Int = PAGE): IssuePage = get(
        "issues", IssuePage.serializer(),
        "q" to query, "limit" to limit.toString(), "offset" to offset.toString(),
        "syntax" to if (text) "text" else null,
        "sort" to "updated:desc",
    )

    suspend fun issue(key: String): IssueView = get("issues/$key/view", IssueView.serializer())

    suspend fun addComment(key: String, text: String): Comment {
        val body = buildJsonObject { put("body", text) }.toString()
        return apiJson.decodeFromString(Comment.serializer(), call(post("issues/$key/comments", body)).body)
    }

    suspend fun boards(project: String): List<Board> =
        get("projects/$project/boards", kotlinx.serialization.builtins.ListSerializer(Board.serializer()))

    suspend fun board(id: String): BoardView = get("boards/$id", BoardView.serializer())

    suspend fun notifications(): List<Notification> =
        get("notifications", kotlinx.serialization.builtins.ListSerializer(Notification.serializer()), "limit" to "100")

    suspend fun markAllRead() { call(post("notifications/read-all", "{}")) }

    private suspend fun <T> get(path: String, ser: kotlinx.serialization.KSerializer<T>, vararg q: Pair<String, String?>): T {
        val url = base.newBuilder().addPathSegments("api/v1/$path").apply { q.forEach { (k, v) -> if (v != null) addQueryParameter(k, v) } }.build()
        return apiJson.decodeFromString(ser, call(Request.Builder().url(url).get()).body)
    }

    private fun post(path: String, json: String) =
        Request.Builder().url(base.newBuilder().addPathSegments("api/v1/$path").build()).post(json.toRequestBody(JSON_TYPE))

    private class Raw(val body: String, val cookie: String?)

    private suspend fun call(rb: Request.Builder, auth: Boolean = true): Raw = withContext(Dispatchers.IO) {
        rb.header("Accept", "application/json")
        if (auth) token()?.let { rb.header("Authorization", "Bearer $it") }
        http.newCall(rb.build()).execute().use { resp -> read(resp) }
    }

    private fun read(resp: Response): Raw {
        val text = resp.body?.string().orEmpty()
        if (!resp.isSuccessful) {
            val e = runCatching { apiJson.decodeFromString(ApiError.serializer(), text) }.getOrNull()
            throw ApiException(resp.code, e?.code.orEmpty(), e?.error?.ifEmpty { null } ?: "HTTP ${resp.code}")
        }
        val cookie = resp.headers("Set-Cookie").firstNotNullOfOrNull { h ->
            h.substringBefore(';').takeIf { it.startsWith("$COOKIE=") }?.removePrefix("$COOKIE=")?.ifEmpty { null }
        }
        return Raw(text, cookie)
    }

    companion object {
        const val PAGE = 50
        val defaultClient: OkHttpClient = OkHttpClient.Builder()
            .connectTimeout(15, TimeUnit.SECONDS).readTimeout(30, TimeUnit.SECONDS).build()
    }
}

/** "tasklane.example.com" and "http://10.0.0.5:8080/" both become a base URL; a bare host gets https. */
fun normalizeServerUrl(raw: String): String {
    val s = raw.trim().trimEnd('/')
    return if (s.startsWith("http://") || s.startsWith("https://")) s else "https://$s"
}
