package org.tasklane.android.ui

import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.*
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.material3.*
import androidx.compose.runtime.*
import androidx.compose.ui.Modifier
import androidx.compose.ui.unit.dp
import org.tasklane.android.data.*

@Composable
fun ProjectsScreen(api: TasklaneApi, onUnauthorized: () -> Unit, onOpen: (String) -> Unit) {
    val loader = rememberLoad(Unit, onUnauthorized) { api.projects().filter { !it.archived } }
    LoadView(loader) { ps ->
        if (ps.isEmpty()) Empty("No projects") else LazyColumn(Modifier.fillMaxSize()) {
            items(ps, key = { it.key }) { p ->
                ListItem(
                    headlineContent = { Text(p.name.ifEmpty { p.key }) },
                    supportingContent = { Text(p.key) },
                    trailingContent = { if (p.mirrored) Chip("Jira mirror", MaterialTheme.colorScheme.secondary) },
                    modifier = Modifier.clickable { onOpen(p.key) },
                )
                HorizontalDivider()
            }
        }
    }
}

/** One project: its boards on top, its issues below. */
@Composable
fun ProjectScreen(api: TasklaneApi, key: String, onUnauthorized: () -> Unit, onOpenIssue: (String) -> Unit, onOpenBoard: (String) -> Unit) {
    val boards = rememberLoad(key, onUnauthorized) { api.boards(key) }
    Column(Modifier.fillMaxSize()) {
        (boards.state as? Load.Ok)?.value?.takeIf { it.isNotEmpty() }?.let { bs ->
            androidx.compose.foundation.lazy.LazyRow(
                contentPadding = PaddingValues(horizontal = 12.dp, vertical = 8.dp),
                horizontalArrangement = Arrangement.spacedBy(8.dp),
            ) {
                items(bs, key = { it.id }) { b -> AssistChip(onClick = { onOpenBoard(b.id) }, label = { Text("${b.name} (${b.kind})") }) }
            }
        }
        IssuesScreen(api, key, onUnauthorized, onOpenIssue)
    }
}
