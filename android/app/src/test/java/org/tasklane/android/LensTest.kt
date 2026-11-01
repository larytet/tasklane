package org.tasklane.android

import org.junit.Assert.*
import org.junit.Test
import org.tasklane.android.ui.Lens

class LensTest {
    @Test fun aShortListIsNotSqueezed() {
        val l = Lens.layout(5, 800f, 2f)
        assertTrue(l.heights.all { it == Lens.ROW_MAX })
        assertEquals(5, l.count)
    }

    @Test fun theWindowTakesAboutTwoThirdsAndTheListFillsTheHeight() {
        val l = Lens.layout(58, 900f, 20f)
        val window = (l.start until l.start + l.count).sumOf { l.heights[it].toDouble() }
        assertEquals(600.0, window, 1.0)
        assertEquals(900.0, l.heights.sumOf { it.toDouble() }, 0.5)
        assertTrue(l.heights.all { it > 0f })
    }

    @Test fun aTallerScreenGetsAWiderWindowNotTallerRows() {
        val small = Lens.layout(200, 500f, 50f)
        val tall = Lens.layout(200, 1500f, 50f)
        assertTrue(tall.count > small.count)
        assertTrue(tall.windowRowHeight <= Lens.ROW_CAP)
    }

    @Test fun rowsGetThinnerWithDistanceFromTheWindow() {
        val l = Lens.layout(100, 900f, 50f)
        val below = l.start + l.count
        assertTrue(l.heights[below] > l.heights[below + 5])
        assertTrue(l.heights[l.start - 1] > l.heights[l.start - 6])
        assertTrue(l.heights[below + 5] >= l.heights[below + 40])
    }

    @Test fun theWindowStaysInsideTheList() {
        for (c in listOf(-5f, 0f, 1f, 57f, 80f)) {
            val l = Lens.layout(58, 900f, c)
            assertTrue(l.start >= 0 && l.start + l.count <= 58)
        }
        assertEquals(0, Lens.layout(58, 900f, 0f).start)
    }

    @Test fun rowAtFindsTheRowUnderAPoint() {
        val l = Lens.layout(58, 900f, 20f)
        for (i in listOf(0, 10, l.start, l.start + 1, 57)) assertEquals(i, l.rowAt(l.tops[i] + l.heights[i] / 2))
        assertEquals(57, l.rowAt(5000f))
    }

    @Test fun emptyInputGivesAnEmptyLayout() {
        assertEquals(0, Lens.layout(0, 800f, 0f).heights.size)
        assertEquals(-1, Lens.layout(0, 800f, 0f).rowAt(10f))
    }
}
