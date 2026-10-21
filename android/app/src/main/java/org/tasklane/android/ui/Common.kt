package org.tasklane.android.ui

import androidx.compose.foundation.layout.*
import androidx.compose.material3.*
import androidx.compose.runtime.*
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.unit.dp
import org.tasklane.android.data.ApiException
import java.time.OffsetDateTime
import java.time.format.DateTimeFormatter
import java.time.format.FormatStyle

sealed interface Load<out T> {
    data object Loading : Load<Nothing>
    data class Ok<T>(val value: T) : Load<T>
    data class Failed(val error: Throwable) : Load<Nothing>
}

/** Runs [block] when [key] changes or [Loader.reload] is called; a 401 is passed to [onUnauthorized]. */
class Loader<T>(val state: Load<T>, val reload: () -> Unit)

@Composable
fun <T> rememberLoad(key: Any?, onUnauthorized: () -> Unit, block: suspend () -> T): Loader<T> {
    var tick by remember { mutableIntStateOf(0) }
    var state by remember(key) { mutableStateOf<Load<T>>(Load.Loading) }
    val unauthorized by rememberUpdatedState(onUnauthorized)
    val current by rememberUpdatedState(block)
    LaunchedEffect(key, tick) {
        try {
            state = Load.Ok(current())
        } catch (e: kotlinx.coroutines.CancellationException) {
            throw e
        } catch (e: Throwable) {
            if (e is ApiException && e.unauthorized) unauthorized()
            state = Load.Failed(e)
        }
    }
    return Loader(state) { tick++ }
}

@Composable
fun <T> LoadView(loader: Loader<T>, content: @Composable (T) -> Unit) {
    when (val s = loader.state) {
        Load.Loading -> Box(Modifier.fillMaxSize(), Alignment.Center) { CircularProgressIndicator() }
        is Load.Failed -> ErrorView(s.error, loader.reload)
        is Load.Ok -> content(s.value)
    }
}

@Composable
fun ErrorView(e: Throwable, retry: () -> Unit) {
    Column(Modifier.fillMaxSize().padding(24.dp), Arrangement.Center, Alignment.CenterHorizontally) {
        Text(e.message ?: "Something went wrong", color = MaterialTheme.colorScheme.error)
        Spacer(Modifier.height(12.dp))
        OutlinedButton(onClick = retry) { Text("Retry") }
    }
}

@Composable
fun Empty(text: String) {
    Box(Modifier.fillMaxSize().padding(24.dp), Alignment.Center) {
        Text(text, color = MaterialTheme.colorScheme.onSurfaceVariant)
    }
}

/** A small coloured label: status category or priority. */
@Composable
fun Chip(text: String, color: Color) {
    if (text.isEmpty()) return
    Surface(color = color.copy(alpha = 0.15f), shape = MaterialTheme.shapes.small) {
        Text(text, Modifier.padding(horizontal = 8.dp, vertical = 2.dp), color = color, style = MaterialTheme.typography.labelSmall)
    }
}

fun categoryColor(category: String) = when (category) {
    "done" -> Color(0xFF2E7D32)
    "in_progress" -> Color(0xFF1565C0)
    else -> Color(0xFF616161)
}

fun priorityColor(priority: String) = when (priority) {
    "highest", "high" -> Color(0xFFC62828)
    "medium" -> Color(0xFFEF6C00)
    else -> Color(0xFF616161)
}

/** "12 Mar 2026, 14:05" in the device's zone and locale; the raw text if it is not an RFC 3339 time. */
fun shortTime(iso: String): String = runCatching {
    OffsetDateTime.parse(iso).atZoneSameInstant(java.time.ZoneId.systemDefault())
        .format(DateTimeFormatter.ofLocalizedDateTime(FormatStyle.MEDIUM, FormatStyle.SHORT))
}.getOrDefault(iso)
