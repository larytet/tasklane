package org.tasklane.android.ui

import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.*
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.material3.*
import androidx.compose.runtime.*
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.ui.Modifier
import androidx.compose.ui.unit.dp
import org.tasklane.android.data.*

/** A board is read-only on the phone: one tab per column, the cards of the chosen column below. */
@Composable
fun BoardScreen(api: TasklaneApi, id: String, onUnauthorized: () -> Unit, onOpenIssue: (String) -> Unit) {
    val loader = rememberLoad(id, onUnauthorized) { api.board(id) }
    LoadView(loader) { v ->
        val cols = v.columns + if (v.unmapped.isNotEmpty()) listOf(Column(name = "Unmapped", cards = v.unmapped, count = v.unmapped.size)) else emptyList()
        if (cols.isEmpty()) { Empty("This board has no columns"); return@LoadView }
        var selected by rememberSaveable(id) { mutableIntStateOf(0) }
        val sel = selected.coerceIn(0, cols.lastIndex)
        Column(Modifier.fillMaxSize()) {
            ScrollableTabRow(sel, edgePadding = 8.dp) {
                cols.forEachIndexed { i, c -> Tab(sel == i, { selected = i }, text = { Text("${c.name} ${c.count}") }) }
            }
            val cards = cols[sel].cards
            if (cards.isEmpty()) Empty("No cards") else LazyColumn(Modifier.fillMaxSize(), contentPadding = PaddingValues(12.dp), verticalArrangement = Arrangement.spacedBy(8.dp)) {
                items(cards, key = { it.key }) { c ->
                    ElevatedCard(Modifier.fillMaxWidth().clickable { onOpenIssue(c.key) }) {
                        Column(Modifier.padding(12.dp), verticalArrangement = Arrangement.spacedBy(4.dp)) {
                            Row(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                                Text(c.key, style = MaterialTheme.typography.labelMedium, color = MaterialTheme.colorScheme.primary)
                                Chip(c.priority, priorityColor(c.priority))
                            }
                            Text(c.summary)
                            if (c.assignee.isNotEmpty()) Text(c.assignee, style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
                        }
                    }
                }
            }
        }
    }
}
