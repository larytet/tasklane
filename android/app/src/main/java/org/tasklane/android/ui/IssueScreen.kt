package org.tasklane.android.ui

import androidx.compose.foundation.layout.*
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.text.selection.SelectionContainer
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.automirrored.filled.Send
import androidx.compose.material3.*
import androidx.compose.runtime.*
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.unit.dp
import kotlinx.coroutines.launch
import org.tasklane.android.data.*

@Composable
fun IssueScreen(api: TasklaneApi, key: String, onUnauthorized: () -> Unit) {
    val loader = rememberLoad(key, onUnauthorized) { api.issue(key) }
    LoadView(loader) { v ->
        Column(Modifier.fillMaxSize()) {
            LazyColumn(Modifier.weight(1f).fillMaxWidth(), contentPadding = PaddingValues(16.dp), verticalArrangement = Arrangement.spacedBy(12.dp)) {
                item { Header(v.issue) }
                if (v.issue.description.isNotBlank()) item {
                    Section("Description") { SelectionContainer { Text(v.issue.description) } }
                }
                item { Fields(v.issue) }
                item { Text("Comments (${v.comments.size})", style = MaterialTheme.typography.titleSmall) }
                if (v.comments.isEmpty()) item { Text("No comments yet", color = MaterialTheme.colorScheme.onSurfaceVariant) }
                items(v.comments, key = { it.id }) { CommentItem(it) }
            }
            if (v.role != "viewer") CommentBar(api, key, onUnauthorized) { loader.reload() }
        }
    }
}

@Composable
private fun Header(i: Issue) {
    Column(verticalArrangement = Arrangement.spacedBy(6.dp)) {
        Row(horizontalArrangement = Arrangement.spacedBy(8.dp), verticalAlignment = Alignment.CenterVertically) {
            Text(i.key, color = MaterialTheme.colorScheme.primary, style = MaterialTheme.typography.labelLarge)
            Chip(i.status, categoryColor(i.statusCategory))
            Chip(i.priority, priorityColor(i.priority))
        }
        SelectionContainer { Text(i.summary, style = MaterialTheme.typography.titleLarge) }
    }
}

@Composable
private fun Fields(i: Issue) {
    Section("Details") {
        Column(verticalArrangement = Arrangement.spacedBy(4.dp)) {
            Field("Type", i.type)
            Field("Assignee", i.assignee?.name?.ifEmpty { i.assignee.email } ?: "Unassigned")
            Field("Reporter", i.reporter?.name?.ifEmpty { i.reporter.email }.orEmpty())
            Field("Labels", i.labels.joinToString(", "))
            Field("Parent", i.parent)
            Field("Estimate", i.estimate?.let { if (it % 1.0 == 0.0) it.toInt().toString() else it.toString() }.orEmpty())
            Field("Created", if (i.created.isEmpty()) "" else shortTime(i.created))
            Field("Updated", if (i.updated.isEmpty()) "" else shortTime(i.updated))
        }
    }
}

@Composable
private fun Field(label: String, value: String) {
    if (value.isEmpty()) return
    Row {
        Text(label, Modifier.width(96.dp), color = MaterialTheme.colorScheme.onSurfaceVariant, style = MaterialTheme.typography.bodyMedium)
        Text(value, style = MaterialTheme.typography.bodyMedium)
    }
}

@Composable
private fun Section(title: String, content: @Composable () -> Unit) {
    Column(verticalArrangement = Arrangement.spacedBy(4.dp)) {
        Text(title, style = MaterialTheme.typography.titleSmall)
        content()
    }
}

@Composable
private fun CommentItem(c: Comment) {
    Card(Modifier.fillMaxWidth(), colors = CardDefaults.cardColors(containerColor = MaterialTheme.colorScheme.surfaceVariant.copy(alpha = 0.4f))) {
        Column(Modifier.padding(12.dp), verticalArrangement = Arrangement.spacedBy(4.dp)) {
            Row(horizontalArrangement = Arrangement.spacedBy(8.dp), verticalAlignment = Alignment.CenterVertically) {
                Text(c.author.name.ifEmpty { c.author.email }, style = MaterialTheme.typography.labelLarge)
                Text(shortTime(c.created), style = MaterialTheme.typography.labelSmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
                if (c.internal) Chip("internal", MaterialTheme.colorScheme.tertiary)
            }
            SelectionContainer { Text(c.body) }
            if (c.reactions.isNotEmpty()) Text(c.reactions.joinToString("  ") { "${it.emoji} ${it.count}" }, style = MaterialTheme.typography.bodySmall)
        }
    }
}

@Composable
private fun CommentBar(api: TasklaneApi, key: String, onUnauthorized: () -> Unit, onPosted: () -> Unit) {
    var text by rememberSaveable(key) { mutableStateOf("") }
    var busy by remember { mutableStateOf(false) }
    var error by remember { mutableStateOf<String?>(null) }
    val scope = rememberCoroutineScope()
    Surface(tonalElevation = 3.dp) {
        Column(Modifier.fillMaxWidth().padding(8.dp).imePadding()) {
            error?.let { Text(it, color = MaterialTheme.colorScheme.error, style = MaterialTheme.typography.bodySmall) }
            Row(verticalAlignment = Alignment.Bottom) {
                OutlinedTextField(text, { text = it }, Modifier.weight(1f), placeholder = { Text("Add a comment") }, maxLines = 4)
                IconButton(
                    onClick = {
                        busy = true; error = null
                        scope.launch {
                            try {
                                api.addComment(key, text.trim()); text = ""; onPosted()
                            } catch (e: kotlinx.coroutines.CancellationException) {
                                throw e
                            } catch (e: Exception) {
                                if (e is ApiException && e.unauthorized) onUnauthorized()
                                error = e.message
                            } finally { busy = false }
                        }
                    },
                    enabled = !busy && text.isNotBlank(),
                ) { Icon(Icons.AutoMirrored.Filled.Send, "Post comment") }
            }
        }
    }
}
