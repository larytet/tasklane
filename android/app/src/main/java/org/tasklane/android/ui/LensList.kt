package org.tasklane.android.ui

import androidx.compose.foundation.background
import androidx.compose.foundation.gestures.awaitEachGesture
import androidx.compose.foundation.gestures.awaitFirstDown
import androidx.compose.foundation.layout.*
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Text
import androidx.compose.runtime.*
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clipToBounds
import androidx.compose.ui.input.pointer.PointerEventPass
import androidx.compose.ui.input.pointer.pointerInput
import androidx.compose.ui.platform.LocalDensity
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.Dp
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import org.tasklane.android.data.IssueRow

/**
 * The issue list as a lens: about two thirds of the screen shows a few consecutive issues in full, and all the others are
 * squeezed above and below, thinner the farther away (single lines, then coloured ticks). Touch or drag outside the
 * highlighted window to slide it there; tap inside it to open that issue.
 */
@Composable
fun LensIssues(rows: List<IssueRow>, status: @Composable () -> Unit, onOpen: (String) -> Unit) {
    var center by rememberSaveable { mutableFloatStateOf(4f) }
    val rowsNow by rememberUpdatedState(rows)
    val open by rememberUpdatedState(onOpen)
    val density = LocalDensity.current.density
    Column(Modifier.fillMaxSize()) {
        BoxWithConstraints(Modifier.weight(1f).fillMaxWidth().clipToBounds()) {
            val layout = Lens.layout(rows.size, maxHeight.value, center)
            val layoutNow by rememberUpdatedState(layout)
            Column(
                Modifier.fillMaxSize().pointerInput(Unit) {
                    awaitEachGesture {
                        val down = awaitFirstDown(pass = PointerEventPass.Initial)
                        var slid = false
                        val first = layoutNow.rowAt(down.position.y / density)
                        if (first >= 0 && !layoutNow.inWindow(first)) { center = first.toFloat(); slid = true }
                        while (true) {
                            val change = awaitPointerEvent(PointerEventPass.Initial).changes.firstOrNull { it.id == down.id } ?: break
                            if (!change.pressed) break
                            val i = layoutNow.rowAt(change.position.y / density)
                            if (i >= 0 && !layoutNow.inWindow(i)) { center = i.toFloat(); slid = true }
                        }
                        if (!slid && first >= 0) rowsNow.getOrNull(first)?.let { open(it.key) }
                    }
                },
            ) {
                rows.forEachIndexed { i, r -> key(r.key) { LensRowItem(r, layout.heights[i].dp, layout.inWindow(i)) } }
            }
        }
        Box(Modifier.fillMaxWidth().padding(6.dp), Alignment.Center) { status() }
    }
}

@Composable
private fun LensRowItem(row: IssueRow, height: Dp, inLens: Boolean) {
    val colors = MaterialTheme.colorScheme
    val base = Modifier.fillMaxWidth().height(height).clipToBounds()
        .background(if (inLens) colors.primaryContainer.copy(alpha = 0.35f) else colors.surface)
    when {
        height.value >= 50f -> Column(base.padding(horizontal = 16.dp), Arrangement.Center) {
            Row(verticalAlignment = Alignment.CenterVertically, horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                Text(row.key, style = MaterialTheme.typography.labelMedium, color = colors.primary)
                Chip(row.status, categoryColor(row.statusCategory))
                Chip(row.priority, priorityColor(row.priority))
            }
            Text(row.summary, Modifier.padding(top = 2.dp), maxLines = if (height.value >= 76f) 2 else 1, overflow = TextOverflow.Ellipsis)
            val meta = listOfNotNull(row.type.ifEmpty { null }, row.assignee.ifEmpty { null }, if (row.comments > 0) "${row.comments} comments" else null)
            if (meta.isNotEmpty() && height.value >= 64f) {
                Text(meta.joinToString(" · "), style = MaterialTheme.typography.bodySmall, color = colors.onSurfaceVariant, maxLines = 1)
            }
        }
        height.value >= 24f -> Row(base.padding(horizontal = 16.dp), Arrangement.spacedBy(8.dp), Alignment.CenterVertically) {
            Text(row.key, style = MaterialTheme.typography.labelMedium, color = colors.primary, maxLines = 1)
            Chip(row.status, categoryColor(row.statusCategory))
            Text(row.summary, style = MaterialTheme.typography.bodyMedium, maxLines = 1, overflow = TextOverflow.Ellipsis)
        }
        height.value >= 10f -> {
            val size = (height.value - 3f).coerceIn(8f, 13f).sp
            Row(base.padding(horizontal = 16.dp), Arrangement.spacedBy(8.dp), Alignment.CenterVertically) {
                Text(row.key, fontSize = size, lineHeight = size, color = colors.primary, maxLines = 1)
                Text(row.summary, fontSize = size, lineHeight = size, maxLines = 1, overflow = TextOverflow.Ellipsis)
            }
        }
        else -> Box(base.padding(bottom = if (height.value >= 3f) 1.dp else 0.dp).background(categoryColor(row.statusCategory).copy(alpha = 0.55f)))
    }
}
