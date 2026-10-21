package org.tasklane.android.data

import kotlinx.serialization.SerialName
import kotlinx.serialization.Serializable

@Serializable
data class User(
    val id: String = "",
    val email: String = "",
    val name: String = "",
    val kind: String = "",
    @SerialName("is_admin") val admin: Boolean = false,
)

@Serializable
data class Project(
    val id: String = "",
    val key: String,
    val name: String = "",
    val type: String = "",
    val archived: Boolean = false,
    val mirrored: Boolean = false,
    val role: String = "",
)

/** One row of the issue list (GET /api/v1/issues). */
@Serializable
data class IssueRow(
    val key: String,
    val summary: String = "",
    val type: String = "",
    val status: String = "",
    @SerialName("status_category") val statusCategory: String = "",
    val priority: String = "",
    val assignee: String = "",
    val project: String = "",
    val labels: List<String> = emptyList(),
    val updated: String = "",
    val comments: Int = 0,
)

@Serializable
data class IssuePage(
    val issues: List<IssueRow> = emptyList(),
    val total: Int = 0,
    @SerialName("total_capped") val totalCapped: Boolean = false,
)

@Serializable
data class Issue(
    val key: String,
    val project: String = "",
    val type: String = "",
    val summary: String = "",
    val description: String = "",
    val status: String = "",
    @SerialName("status_category") val statusCategory: String = "",
    val priority: String = "",
    val reporter: User? = null,
    val assignee: User? = null,
    val labels: List<String> = emptyList(),
    val parent: String = "",
    val created: String = "",
    val updated: String = "",
    val estimate: Double? = null,
)

@Serializable
data class Reaction(
    val emoji: String,
    val count: Int = 0,
    val mine: Boolean = false,
)

@Serializable
data class Comment(
    val id: String,
    val author: User = User(),
    val body: String = "",
    val internal: Boolean = false,
    val created: String = "",
    val edited: String? = null,
    val reactions: List<Reaction> = emptyList(),
)

@Serializable
data class Attachment(
    val id: String = "",
    val name: String = "",
    val filename: String = "",
    val size: Long = 0,
)

@Serializable
data class IssueView(
    val issue: Issue,
    val comments: List<Comment> = emptyList(),
    val attachments: List<Attachment> = emptyList(),
    val role: String = "",
)

@Serializable
data class Board(
    val id: String,
    val project: String = "",
    val name: String = "",
    val kind: String = "",
)

@Serializable
data class Card(
    val key: String,
    val summary: String = "",
    val type: String = "",
    val status: String = "",
    val priority: String = "",
    val assignee: String = "",
    val labels: List<String> = emptyList(),
    val category: String = "",
)

@Serializable
data class Column(
    val id: String = "",
    val name: String = "",
    val cards: List<Card> = emptyList(),
    val count: Int = 0,
)

@Serializable
data class BoardView(
    val board: Board,
    val columns: List<Column> = emptyList(),
    val unmapped: List<Card> = emptyList(),
)

@Serializable
data class Notification(
    val id: Long,
    val issue: String = "",
    val summary: String = "",
    val kind: String = "",
    val what: String = "",
    val body: String = "",
    val actor: String = "",
    val state: String = "",
    val at: String = "",
)

/** The answer to POST /api/v1/login: either a signed-in user or a second factor to supply. */
@Serializable
data class LoginReply(
    val id: String = "",
    val email: String = "",
    val name: String = "",
    @SerialName("mfa_required") val mfaRequired: Boolean = false,
    @SerialName("mfa_token") val mfaToken: String = "",
    val totp: Boolean = false,
    // a passkey-only account cannot finish a sign-in without the platform WebAuthn flow
    val passkey: kotlinx.serialization.json.JsonElement? = null,
)

@Serializable
data class ApiError(val error: String = "", val code: String = "")
