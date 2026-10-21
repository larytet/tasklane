package org.tasklane.android

import kotlinx.coroutines.test.runTest
import org.junit.Assert.*
import org.junit.Assume.assumeTrue
import org.junit.Test
import org.tasklane.android.data.*

/**
 * Runs the real client against a real server. Skipped unless TASKLANE_IT_URL, TASKLANE_IT_EMAIL and TASKLANE_IT_PASSWORD
 * are set. The account must be able to create projects; the test makes its own project, so use a throwaway server:
 *   TASKLANE_IT_URL=http://127.0.0.1:18091 TASKLANE_IT_EMAIL=... TASKLANE_IT_PASSWORD=... ./gradlew :app:testDebugUnitTest
 */
class LiveServerTest {
    @Test fun signInBrowseAndComment() = runTest {
        val url = System.getenv("TASKLANE_IT_URL")
        assumeTrue("TASKLANE_IT_URL not set", url != null)
        var token: String? = null
        val api = TasklaneApi(url!!, { token })

        val bad = runCatching { api.login(System.getenv("TASKLANE_IT_EMAIL"), "definitely-wrong") }.exceptionOrNull() as ApiException
        assertEquals(401, bad.status)

        val me = api.login(System.getenv("TASKLANE_IT_EMAIL"), System.getenv("TASKLANE_IT_PASSWORD")) as LoginResult.SignedIn
        token = me.token
        assertTrue(api.me().id.isNotEmpty())

        val demo = api.projects().firstOrNull { it.key == "SHOP" }
        assumeTrue("seed a SHOP project with issue SHOP-1 and a board first", demo != null)

        val page = api.issues("project = \"SHOP\"", text = false, offset = 0)
        assertTrue(page.total >= 1)
        assertTrue(api.issues("3DS", text = true, offset = 0).issues.any { it.key == "SHOP-1" })

        val view = api.issue("SHOP-1")
        assertEquals("SHOP-1", view.issue.key)
        val before = view.comments.size
        api.addComment("SHOP-1", "from the Android client test")
        assertEquals(before + 1, api.issue("SHOP-1").comments.size)

        val board = api.boards("SHOP").first()
        assertTrue(api.board(board.id).columns.flatMap { it.cards }.any { it.key == "SHOP-1" })
        api.notifications()

        api.logout()
        val after = runCatching { api.me() }.exceptionOrNull() as ApiException
        assertTrue("a signed-out token must be refused", after.unauthorized)
    }
}
