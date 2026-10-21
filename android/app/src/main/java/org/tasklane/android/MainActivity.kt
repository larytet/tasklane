package org.tasklane.android

import android.os.Bundle
import androidx.activity.ComponentActivity
import androidx.activity.compose.setContent
import androidx.compose.foundation.isSystemInDarkTheme
import androidx.compose.foundation.layout.padding
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.automirrored.filled.ArrowBack
import androidx.compose.material.icons.automirrored.filled.List
import androidx.compose.material.icons.filled.Folder
import androidx.compose.material.icons.filled.MoreVert
import androidx.compose.material.icons.filled.Notifications
import androidx.compose.material3.*
import androidx.compose.runtime.*
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.vector.ImageVector
import androidx.navigation.NavDestination.Companion.hierarchy
import androidx.navigation.NavGraph.Companion.findStartDestination
import androidx.navigation.compose.*
import kotlinx.coroutines.launch
import org.tasklane.android.data.*
import org.tasklane.android.ui.*

class MainActivity : ComponentActivity() {
    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        val session = Session(applicationContext)
        setContent {
            // follows the device's day/night setting
            MaterialTheme(colorScheme = if (isSystemInDarkTheme()) darkColorScheme() else lightColorScheme()) {
                Surface(color = MaterialTheme.colorScheme.background) { TasklaneApp(session) }
            }
        }
    }
}

private enum class Tab(val route: String, val label: String, val icon: ImageVector) {
    Issues("issues", "Issues", Icons.AutoMirrored.Filled.List),
    Projects("projects", "Projects", Icons.Filled.Folder),
    Inbox("inbox", "Inbox", Icons.Filled.Notifications),
}

@OptIn(ExperimentalMaterial3Api::class)
@Composable
private fun TasklaneApp(session: Session) {
    var signedIn by remember { mutableStateOf(session.signedIn) }
    if (!signedIn) {
        LoginScreen(session.server) { server, token, user ->
            session.server = server; session.token = token; session.userName = user.name.ifEmpty { user.email }
            signedIn = true
        }
        return
    }

    val api = remember(session.server) { TasklaneApi(session.server, { session.token }) }
    val scope = rememberCoroutineScope()
    val signOut: () -> Unit = {
        scope.launch { api.logout(); session.signOut(); signedIn = false }
    }
    // a token the server no longer accepts (expired, revoked, signed out elsewhere) goes back to the sign-in screen
    val expired: () -> Unit = { session.signOut(); signedIn = false }

    val nav = rememberNavController()
    val entry by nav.currentBackStackEntryAsState()
    val dest = entry?.destination
    val isTab = Tab.entries.any { t -> dest?.hierarchy?.any { it.route == t.route } == true }
    val title = when {
        dest?.route?.startsWith("issue/") == true -> entry?.arguments?.getString("key").orEmpty()
        dest?.route?.startsWith("project/") == true -> entry?.arguments?.getString("key").orEmpty()
        dest?.route?.startsWith("board/") == true -> "Board"
        else -> Tab.entries.firstOrNull { it.route == dest?.route }?.label ?: "Tasklane"
    }

    Scaffold(
        topBar = {
            TopAppBar(
                title = { Text(title) },
                navigationIcon = {
                    if (!isTab) IconButton({ nav.popBackStack() }) { Icon(Icons.AutoMirrored.Filled.ArrowBack, "Back") }
                },
                actions = {
                    var menu by remember { mutableStateOf(false) }
                    IconButton({ menu = true }) { Icon(Icons.Filled.MoreVert, "Menu") }
                    DropdownMenu(menu, { menu = false }) {
                        DropdownMenuItem(text = { Text("Signed in as ${session.userName}") }, onClick = {}, enabled = false)
                        DropdownMenuItem(text = { Text("Sign out") }, onClick = { menu = false; signOut() })
                    }
                },
            )
        },
        bottomBar = {
            if (isTab) NavigationBar {
                Tab.entries.forEach { t ->
                    NavigationBarItem(
                        selected = dest?.hierarchy?.any { it.route == t.route } == true,
                        onClick = { nav.navigate(t.route) { popUpTo(nav.graph.findStartDestination().id) { saveState = true }; launchSingleTop = true; restoreState = true } },
                        icon = { Icon(t.icon, null) }, label = { Text(t.label) },
                    )
                }
            }
        },
    ) { pad ->
        NavHost(nav, Tab.Issues.route, Modifier.padding(pad)) {
            composable(Tab.Issues.route) { IssuesScreen(api, null, expired) { nav.navigate("issue/$it") } }
            composable(Tab.Projects.route) { ProjectsScreen(api, expired) { nav.navigate("project/$it") } }
            composable(Tab.Inbox.route) { InboxScreen(api, expired) { nav.navigate("issue/$it") } }
            composable("issue/{key}") { IssueScreen(api, it.arguments!!.getString("key")!!, expired) }
            composable("project/{key}") {
                ProjectScreen(api, it.arguments!!.getString("key")!!, expired, { k -> nav.navigate("issue/$k") }, { id -> nav.navigate("board/$id") })
            }
            composable("board/{id}") { BoardScreen(api, it.arguments!!.getString("id")!!, expired) { k -> nav.navigate("issue/$k") } }
        }
    }
}
