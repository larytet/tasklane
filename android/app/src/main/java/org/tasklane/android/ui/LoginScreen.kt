package org.tasklane.android.ui

import androidx.compose.foundation.layout.*
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.material3.*
import androidx.compose.runtime.*
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.text.input.KeyboardType
import androidx.compose.ui.text.input.PasswordVisualTransformation
import androidx.compose.ui.unit.dp
import kotlinx.coroutines.launch
import org.tasklane.android.data.*

@Composable
fun LoginScreen(initialServer: String, onSignedIn: (server: String, token: String, user: User) -> Unit) {
    var server by rememberSaveable { mutableStateOf(initialServer) }
    var email by rememberSaveable { mutableStateOf("") }
    var password by rememberSaveable { mutableStateOf("") }
    var code by rememberSaveable { mutableStateOf("") }
    var mfaToken by rememberSaveable { mutableStateOf<String?>(null) }
    var busy by remember { mutableStateOf(false) }
    var error by remember { mutableStateOf<String?>(null) }
    val scope = rememberCoroutineScope()

    fun submit() {
        if (busy) return
        busy = true; error = null
        scope.launch {
            try {
                val url = normalizeServerUrl(server)
                val api = TasklaneApi(url, { null })
                val mt = mfaToken
                val r = if (mt == null) api.login(email.trim(), password) else api.loginMfa(mt, code.trim())
                when (r) {
                    is LoginResult.NeedsCode -> { mfaToken = r.mfaToken; code = "" }
                    is LoginResult.SignedIn -> onSignedIn(url, r.token, r.user)
                }
            } catch (e: IllegalArgumentException) {
                error = "That is not a valid server address"
            } catch (e: Exception) {
                error = e.message ?: "Could not sign in"
            } finally {
                busy = false
            }
        }
    }

    Column(
        Modifier.fillMaxSize().padding(24.dp).imePadding(),
        Arrangement.Center, Alignment.CenterHorizontally,
    ) {
        Text("Tasklane", style = MaterialTheme.typography.headlineLarge)
        Spacer(Modifier.height(24.dp))
        if (mfaToken == null) {
            OutlinedTextField(server, { server = it }, Modifier.fillMaxWidth(), label = { Text("Server") },
                placeholder = { Text("tasklane.example.com") }, singleLine = true,
                keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Uri))
            Spacer(Modifier.height(8.dp))
            OutlinedTextField(email, { email = it }, Modifier.fillMaxWidth(), label = { Text("Email") }, singleLine = true,
                keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Email))
            Spacer(Modifier.height(8.dp))
            OutlinedTextField(password, { password = it }, Modifier.fillMaxWidth(), label = { Text("Password") }, singleLine = true,
                visualTransformation = PasswordVisualTransformation(),
                keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Password))
        } else {
            Text("Enter the code from your authenticator app")
            Spacer(Modifier.height(8.dp))
            OutlinedTextField(code, { code = it }, Modifier.fillMaxWidth(), label = { Text("Code") }, singleLine = true,
                keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.NumberPassword))
        }
        error?.let { Spacer(Modifier.height(8.dp)); Text(it, color = MaterialTheme.colorScheme.error) }
        Spacer(Modifier.height(16.dp))
        Button(::submit, Modifier.fillMaxWidth(), enabled = !busy && server.isNotBlank() &&
            (if (mfaToken == null) email.isNotBlank() && password.isNotEmpty() else code.isNotBlank())) {
            if (busy) CircularProgressIndicator(Modifier.size(18.dp), strokeWidth = 2.dp)
            else Text(if (mfaToken == null) "Sign in" else "Verify")
        }
        if (mfaToken != null) TextButton({ mfaToken = null; error = null }) { Text("Back") }
    }
}
