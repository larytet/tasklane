package org.tasklane.android

import kotlinx.coroutines.test.runTest
import okhttp3.mockwebserver.MockResponse
import okhttp3.mockwebserver.MockWebServer
import org.junit.After
import org.junit.Assert.*
import org.junit.Before
import org.junit.Test
import org.tasklane.android.data.*

class ApiTest {
    private lateinit var server: MockWebServer
    private var token: String? = null
    private lateinit var api: TasklaneApi

    @Before fun setUp() {
        server = MockWebServer().also { it.start() }
        api = TasklaneApi(server.url("/").toString(), { token })
    }

    @After fun tearDown() = server.shutdown()

    @Test fun loginTakesTheTokenFromTheSessionCookie() = runTest {
        server.enqueue(MockResponse().setHeader("Set-Cookie", "tl_session=abc123; Path=/; HttpOnly")
            .setBody("""{"id":"u1","email":"a@b.c","name":"Ann","kind":"human","is_admin":false}"""))
        val r = api.login("a@b.c", "pw") as LoginResult.SignedIn
        assertEquals("abc123", r.token)
        assertEquals("Ann", r.user.name)
        val req = server.takeRequest()
        assertEquals("/api/v1/login", req.path)
        assertNull("sign-in must not send a stale token", req.getHeader("Authorization"))
        assertTrue(req.body.readUtf8().contains("\"email\":\"a@b.c\""))
    }

    @Test fun loginAsksForTheSecondFactorThenFinishes() = runTest {
        server.enqueue(MockResponse().setBody("""{"mfa_required":true,"mfa_token":"m1","totp":true}"""))
        assertEquals(LoginResult.NeedsCode("m1"), api.login("a@b.c", "pw"))
        server.enqueue(MockResponse().setHeader("Set-Cookie", "tl_session=t2; Path=/").setBody("""{"id":"u1","name":"Ann"}"""))
        assertEquals("t2", (api.loginMfa("m1", "123456") as LoginResult.SignedIn).token)
        server.takeRequest()
        val req = server.takeRequest()
        assertEquals("/api/v1/login/mfa", req.path)
        assertTrue(req.body.readUtf8().contains("\"mfa_token\":\"m1\""))
    }

    @Test fun passkeyOnlyAccountIsExplained() = runTest {
        server.enqueue(MockResponse().setBody("""{"mfa_required":true,"mfa_token":"m1","totp":false,"passkey":{"challenge":"x"}}"""))
        val e = runCatching { api.login("a@b.c", "pw") }.exceptionOrNull() as ApiException
        assertTrue(e.message!!.contains("passkey"))
    }

    @Test fun errorsCarryTheServersWordsAndStatus() = runTest {
        server.enqueue(MockResponse().setResponseCode(401).setBody("""{"error":"wrong email or password","code":"bad_credentials"}"""))
        val e = runCatching { api.login("a@b.c", "x") }.exceptionOrNull() as ApiException
        assertEquals(401, e.status); assertEquals("bad_credentials", e.code); assertEquals("wrong email or password", e.message)
        assertTrue(e.unauthorized)
    }

    @Test fun nonJsonErrorFallsBackToTheStatus() = runTest {
        token = "t"
        server.enqueue(MockResponse().setResponseCode(502).setBody("<html>bad gateway</html>"))
        val e = runCatching { api.projects() }.exceptionOrNull() as ApiException
        assertEquals("HTTP 502", e.message)
    }

    @Test fun requestsCarryTheBearerToken() = runTest {
        token = "tok"
        server.enqueue(MockResponse().setBody("""[{"id":"p1","key":"ABC","name":"Alpha","type":"software","role":"member"}]"""))
        val ps = api.projects()
        assertEquals("ABC", ps.single().key)
        assertEquals("Bearer tok", server.takeRequest().getHeader("Authorization"))
    }

    @Test fun issueListSendsQueryPagingAndTextSyntax() = runTest {
        token = "t"
        server.enqueue(MockResponse().setBody("""{"issues":[{"key":"ABC-1","summary":"Pay","status":"Open","status_category":"todo","priority":"high","project":"ABC","labels":[],"updated":"2026-03-12T10:00:00Z","comments":2,"unknown_future_field":1}],"total":120,"total_capped":false}"""))
        val page = api.issues("pay \"now\"", text = true, offset = 50)
        assertEquals(120, page.total); assertEquals("ABC-1", page.issues.single().key); assertEquals(2, page.issues.single().comments)
        val url = server.takeRequest().requestUrl!!
        assertEquals("pay \"now\"", url.queryParameter("q"))
        assertEquals("text", url.queryParameter("syntax"))
        assertEquals("50", url.queryParameter("offset"))
        assertEquals("updated:desc", url.queryParameter("sort"))
    }

    @Test fun tqlQueryOmitsTheSyntaxParameter() = runTest {
        token = "t"
        server.enqueue(MockResponse().setBody("""{"issues":[],"total":0}"""))
        api.issues("project = \"ABC\"", text = false, offset = 0)
        assertNull(server.takeRequest().requestUrl!!.queryParameter("syntax"))
    }

    @Test fun issueViewParsesNullsAndComments() = runTest {
        token = "t"
        server.enqueue(MockResponse().setBody("""{"issue":{"key":"ABC-1","summary":"S","status":"Open","status_category":"todo","priority":"low","reporter":{"id":"u","email":"r@x","name":"Rae"},"assignee":null,"labels":null,"estimate":null},
            "comments":[{"id":"c1","author":{"id":"u","email":"r@x","name":"Rae"},"body":"hi","internal":true,"created":"2026-03-12T10:00:00Z","reactions":[{"emoji":"👍","count":2,"names":["a","b"],"mine":true}]}],
            "transitions":[],"attachments":[],"role":"member"}"""))
        val v = api.issue("ABC-1")
        assertNull(v.issue.assignee); assertEquals(emptyList<String>(), v.issue.labels)
        assertTrue(v.comments.single().internal); assertEquals(2, v.comments.single().reactions.single().count)
    }

    @Test fun commentPostsTheBody() = runTest {
        token = "t"
        server.enqueue(MockResponse().setResponseCode(201).setBody("""{"id":"c2","author":{"id":"u","name":"Ann"},"body":"ok","created":"2026-03-12T10:00:00Z"}"""))
        assertEquals("c2", api.addComment("ABC-1", "ok").id)
        val req = server.takeRequest()
        assertEquals("POST", req.method); assertEquals("/api/v1/issues/ABC-1/comments", req.path)
        assertEquals("""{"body":"ok"}""", req.body.readUtf8())
    }

    @Test fun boardParsesColumnsAndCards() = runTest {
        token = "t"
        server.enqueue(MockResponse().setBody("""{"board":{"id":"b1","project":"ABC","name":"Main","kind":"kanban"},"columns":[{"id":"c","name":"To do","statuses":["Open"],"cards":[{"key":"ABC-1","summary":"S","status":"Open","priority":"low","labels":[],"rank":"a","category":"todo"}],"count":1}],"unmapped":[]}"""))
        val b = api.board("b1")
        assertEquals("To do", b.columns.single().name); assertEquals("ABC-1", b.columns.single().cards.single().key)
    }

    @Test fun logoutSendsTheTokenAsTheCookieSoTheServerRevokesIt() = runTest {
        token = "tok9"
        server.enqueue(MockResponse().setBody("""{"ok":true}"""))
        api.logout()
        val req = server.takeRequest()
        assertEquals("/api/v1/logout", req.path)
        assertEquals("tl_session=tok9", req.getHeader("Cookie"))
        assertEquals("tasklane", req.getHeader("X-Requested-With"))
    }

    @Test fun serverUrlsAreNormalized() {
        assertEquals("https://t.example.com", normalizeServerUrl(" t.example.com/ "))
        assertEquals("http://10.0.0.5:8080", normalizeServerUrl("http://10.0.0.5:8080/"))
    }
}
