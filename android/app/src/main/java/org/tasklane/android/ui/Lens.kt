package org.tasklane.android.ui

import kotlin.math.exp
import kotlin.math.min
import kotlin.math.roundToInt

/**
 * Where each row of a "lens" list sits: a window of consecutive rows drawn at full size, taking about two thirds of the
 * height, and every other row squeezed into the remaining third, the farther from the window the thinner.
 * [heights] are in dp, in row order, and add up to the available height (or less when the list is short).
 */
class LensLayout(val heights: FloatArray, val start: Int, val count: Int, val windowRowHeight: Float) {
    val tops = FloatArray(heights.size).also { var y = 0f; for (i in heights.indices) { it[i] = y; y += heights[i] } }

    fun inWindow(i: Int) = i >= start && i < start + count

    /** The row drawn at [y] dp from the top (the first or last row when [y] is outside the list). */
    fun rowAt(y: Float): Int {
        if (heights.isEmpty()) return -1
        var i = heights.size - 1
        for (k in heights.indices) if (y < tops[k] + heights[k]) { i = k; break }
        return i
    }
}

object Lens {
    /** A row in the window is at most this tall, and the window has as many of them as fit in [WINDOW_SHARE]. */
    const val ROW_MAX = 88f
    const val ROW_CAP = 96f
    const val WINDOW_SHARE = 2f / 3f
    /** The thinnest row is this share of the way between nothing and a row just outside the window. */
    const val MIN_WEIGHT = 0.1f
    /** How many rows it takes for the squeeze to reach its maximum. */
    const val FALLOFF = 3f

    /** [n] rows in [height] dp with the window centred, as far as the list allows, on row [center]. */
    fun layout(n: Int, height: Float, center: Float): LensLayout {
        if (n <= 0 || height <= 0f) return LensLayout(FloatArray(0), 0, 0, 0f)
        if (n * ROW_MAX <= height) return LensLayout(FloatArray(n) { ROW_MAX }, 0, n, ROW_MAX)
        val k = (WINDOW_SHARE * height / ROW_MAX).roundToInt().coerceIn(1, n)
        val start = (center - (k - 1) / 2f).roundToInt().coerceIn(0, n - k)
        val hw = min(WINDOW_SHARE * height / k, ROW_CAP)
        val weights = FloatArray(n) { i ->
            if (i in start until start + k) 0f else {
                val d = if (i < start) start - i else i - (start + k - 1)
                val x = (d - 1) / FALLOFF
                MIN_WEIGHT + (1 - MIN_WEIGHT) * exp(-(x * x))
            }
        }
        val sum = weights.sum()
        val outside = height - k * hw
        val heights = FloatArray(n) { i -> if (i in start until start + k) hw else if (sum > 0f) outside * weights[i] / sum else 0f }
        return LensLayout(heights, start, k, hw)
    }
}
