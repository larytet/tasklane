package org.tasklane.android.ui

import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.*
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.lazy.rememberLazyListState
import androidx.compose.foundation.text.KeyboardActions
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.Clear
import androidx.compose.material.icons.filled.Search
import androidx.compose.material3.*
import androidx.compose.runtime.*
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.text.input.ImeAction
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import org.tasklane.android.data.*

/** Issue list with a search box; [project] narrows it to one project. Pages of 50 load as the list scrolls. */
@Composable
fun IssuesScreen(api: TasklaneApi, project: String?, onUnauthorized: () -> Unit, onOpen: (String) -> Unit) {
    var text by rememberSaveable(project) { mutableStateOf("") }
    var submitted by rememberSaveable(project) { mutableStateOf("") }
    val q = remember(project, submitted) {
        buildList {
            if (project != null) add("project = \"$project\"")
            if (submitted.isNotBlank()) add(submitted.trim())
        }
    }
    Column(Modifier.fillMaxSize()) {
        OutlinedTextField(
            text, { text = it },
            Modifier.fillMaxWidth().padding(horizontal = 12.dp, vertical = 8.dp),
            placeholder = { Text("Search issues") }, singleLine = true,
            leadingIcon = { Icon(Icons.Default.Search, null) },
            trailingIcon = { if (text.isNotEmpty()) IconButton({ text = ""; submitted = "" }) { Icon(Icons.Default.Clear, "Clear") } },
            keyboardOptions = KeyboardOptions(imeAction = ImeAction.Search),
            keyboardActions = KeyboardActions(onSearch = { submitted = text }),
        )
        PagedIssues(api, q, onUnauthorized, onOpen)
    }
}

@Composable
private fun PagedIssues(api: TasklaneApi, filters: List<String>, onUnauthorized: () -> Unit, onOpen: (String) -> Unit) {
    // the project clause is TQL and the search words are plain text, so they are joined as TQL only when both are present
    val project = filters.firstOrNull { it.startsWith("project = ") }
    val words = filters.firstOrNull { !it.startsWith("project = ") }.orEmpty()
    val query = when {
        project != null && words.isNotEmpty() -> "$project AND text ~ \"${words.replace("\"", "\\\"")}\""
        project != null -> project
        else -> words
    }
    val asText = project == null && words.isNotEmpty()

    var rows by remember(query) { mutableStateOf<List<IssueRow>>(emptyList()) }
    var total by remember(query) { mutableIntStateOf(0) }
    var failure by remember(query) { mutableStateOf<Throwable?>(null) }
    var loading by remember(query) { mutableStateOf(true) }
    var tick by remember { mutableIntStateOf(0) }
    val unauthorized by rememberUpdatedState(onUnauthorized)

    suspend fun loadMore() {
        loading = true
        try {
            val page = api.issues(query, asText, rows.size)
            rows = rows + page.issues
            total = page.total
            failure = null
        } catch (e: kotlinx.coroutines.CancellationException) {
            throw e
        } catch (e: Throwable) {
            if (e is ApiException && e.unauthorized) unauthorized()
            failure = e
        } finally {
            loading = false
        }
    }
    LaunchedEffect(query, tick) { if (rows.isEmpty()) loadMore() }

    val listState = rememberLazyListState()
    val nearEnd by remember { derivedStateOf { val last = listState.layoutInfo.visibleItemsInfo.lastOrNull()?.index ?: 0; last >= rows.size - 5 } }
    LaunchedEffect(nearEnd, rows.size, loading) {
        if (nearEnd && !loading && failure == null && rows.isNotEmpty() && rows.size < total) loadMore()
    }

    when {
        rows.isEmpty() && loading -> Box(Modifier.fillMaxSize(), Alignment.Center) { CircularProgressIndicator() }
        rows.isEmpty() && failure != null -> ErrorView(failure!!) { tick++ }
        rows.isEmpty() -> Empty("No issues")
        else -> LazyColumn(Modifier.fillMaxSize(), listState) {
            items(rows, key = { it.key }) { IssueRowItem(it) { onOpen(it.key) } }
            item {
                Box(Modifier.fillMaxWidth().padding(12.dp), Alignment.Center) {
                    when {
                        failure != null -> TextButton({ failure = null }) { Text("Could not load more. Retry") }
                        loading -> CircularProgressIndicator(Modifier.size(24.dp), strokeWidth = 2.dp)
                        else -> Text("$total issues", style = MaterialTheme.typography.labelSmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
                    }
                }
            }
        }
    }
}

@Composable
fun IssueRowItem(row: IssueRow, onClick: () -> Unit) {
    Column(Modifier.fillMaxWidth().clickable(onClick = onClick).padding(horizontal = 16.dp, vertical = 10.dp)) {
        Row(verticalAlignment = Alignment.CenterVertically, horizontalArrangement = Arrangement.spacedBy(8.dp)) {
            Text(row.key, style = MaterialTheme.typography.labelMedium, color = MaterialTheme.colorScheme.primary)
            Chip(row.status, categoryColor(row.statusCategory))
            Chip(row.priority, priorityColor(row.priority))
        }
        Text(row.summary, Modifier.padding(top = 2.dp), maxLines = 2, overflow = TextOverflow.Ellipsis)
        val meta = listOfNotNull(row.type.ifEmpty { null }, row.assignee.ifEmpty { null }, if (row.comments > 0) "${row.comments} comments" else null)
        if (meta.isNotEmpty()) Text(meta.joinToString(" · "), style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
    }
    HorizontalDivider()
}
