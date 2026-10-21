package org.tasklane.android.ui

import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.*
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.material3.*
import androidx.compose.runtime.*
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import kotlinx.coroutines.launch
import org.tasklane.android.data.*

@Composable
fun InboxScreen(api: TasklaneApi, onUnauthorized: () -> Unit, onOpenIssue: (String) -> Unit) {
    val loader = rememberLoad(Unit, onUnauthorized) { api.notifications() }
    val scope = rememberCoroutineScope()
    LoadView(loader) { ns ->
        Column(Modifier.fillMaxSize()) {
            if (ns.any { it.state == "unread" }) Row(Modifier.fillMaxWidth().padding(horizontal = 8.dp), horizontalArrangement = Arrangement.End) {
                TextButton({ scope.launch { runCatching { api.markAllRead() }; loader.reload() } }) { Text("Mark all read") }
            }
            if (ns.isEmpty()) Empty("Nothing new") else LazyColumn(Modifier.fillMaxSize()) {
                items(ns, key = { it.id }) { n ->
                    val unread = n.state == "unread"
                    ListItem(
                        overlineContent = { Text("${n.issue} · ${n.actor}") },
                        headlineContent = { Text(n.summary, fontWeight = if (unread) FontWeight.Bold else null, maxLines = 1, overflow = TextOverflow.Ellipsis) },
                        supportingContent = {
                            Column {
                                Text(n.what.ifEmpty { n.kind }, maxLines = 2, overflow = TextOverflow.Ellipsis)
                                Text(shortTime(n.at), style = MaterialTheme.typography.labelSmall)
                            }
                        },
                        trailingContent = { if (unread) Badge() },
                        modifier = Modifier.clickable { onOpenIssue(n.issue) },
                    )
                    HorizontalDivider()
                }
            }
        }
    }
}
