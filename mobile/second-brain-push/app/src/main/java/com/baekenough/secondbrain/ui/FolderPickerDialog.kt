package com.baekenough.secondbrain.ui

import android.os.Bundle
import android.view.LayoutInflater
import android.view.View
import android.view.ViewGroup
import android.widget.Toast
import androidx.fragment.app.DialogFragment
import androidx.recyclerview.widget.DiffUtil
import androidx.recyclerview.widget.LinearLayoutManager
import androidx.recyclerview.widget.ListAdapter
import androidx.recyclerview.widget.RecyclerView
import com.baekenough.secondbrain.R
import com.baekenough.secondbrain.databinding.DialogFolderPickerBinding
import com.baekenough.secondbrain.databinding.ItemFolderBinding
import java.io.File

/**
 * A file-system folder browser dialog that lets the user navigate directories and
 * select a folder to use as the recording path override.
 *
 * Navigation stays within [ROOT_DIR] and its descendants. The dialog returns an
 * absolute path string via [onFolderSelected]; it does NOT use SAF/content:// URIs,
 * keeping it compatible with the existing [PathDetector] / [RecordingScanner] pipeline.
 *
 * Usage:
 * ```kotlin
 * FolderPickerDialog.newInstance("/storage/emulated/0/Recordings").apply {
 *     onFolderSelected = { path -> /* save path */ }
 * }.show(childFragmentManager, FolderPickerDialog.TAG)
 * ```
 */
class FolderPickerDialog : DialogFragment() {

    companion object {
        const val TAG = "FolderPickerDialog"
        private const val ARG_START_PATH = "start_path"

        /** Navigating above this directory is not allowed. */
        private const val ROOT_DIR = "/storage/emulated/0"

        /** Default start directory — the standard Samsung recordings root. */
        private const val DEFAULT_START = "/storage/emulated/0/Recordings"

        fun newInstance(startPath: String = DEFAULT_START): FolderPickerDialog =
            FolderPickerDialog().apply {
                arguments = Bundle().apply { putString(ARG_START_PATH, startPath) }
            }
    }

    /** Called when the user confirms a folder selection. Receives the absolute path. */
    var onFolderSelected: ((String) -> Unit)? = null

    private var _binding: DialogFolderPickerBinding? = null
    private val binding get() = _binding!!

    private lateinit var adapter: FolderAdapter

    /** The directory currently shown in the list. */
    private lateinit var currentDir: File

    override fun onCreateView(
        inflater: LayoutInflater,
        container: ViewGroup?,
        savedInstanceState: Bundle?,
    ): View {
        _binding = DialogFolderPickerBinding.inflate(inflater, container, false)
        return binding.root
    }

    override fun onViewCreated(view: View, savedInstanceState: Bundle?) {
        super.onViewCreated(view, savedInstanceState)

        // Determine starting directory: prefer ARG, fall back to DEFAULT_START, then ROOT_DIR
        val startPath = arguments?.getString(ARG_START_PATH) ?: DEFAULT_START
        val startDir = resolveStartDir(startPath)
        currentDir = startDir

        adapter = FolderAdapter { folder -> navigateTo(folder) }
        binding.recyclerFolders.layoutManager = LinearLayoutManager(requireContext())
        binding.recyclerFolders.adapter = adapter

        binding.btnUp.setOnClickListener { navigateUp() }
        binding.btnSelectFolder.setOnClickListener { confirmSelection() }
        binding.btnCancel.setOnClickListener { dismiss() }

        loadDirectory(currentDir)
    }

    override fun onStart() {
        super.onStart()
        // Full-width dialog
        dialog?.window?.setLayout(
            ViewGroup.LayoutParams.MATCH_PARENT,
            ViewGroup.LayoutParams.WRAP_CONTENT,
        )
    }

    override fun onDestroyView() {
        super.onDestroyView()
        _binding = null
    }

    // ── Navigation ─────────────────────────────────────────────────────────

    private fun navigateTo(folder: File) {
        currentDir = folder
        loadDirectory(folder)
    }

    private fun navigateUp() {
        val parent = currentDir.parentFile ?: return
        // Do not navigate above the root boundary
        if (!currentDir.canonicalPath.startsWith(ROOT_DIR)) return
        if (currentDir.canonicalPath == ROOT_DIR) return
        currentDir = parent
        loadDirectory(parent)
    }

    private fun loadDirectory(dir: File) {
        binding.tvCurrentPath.text = dir.absolutePath
        // Update "up" button state: disable at root boundary
        val atRoot = dir.canonicalPath == ROOT_DIR
        binding.btnUp.isEnabled = !atRoot

        val subDirs = try {
            dir.listFiles { f -> f.isDirectory }
                ?.sortedBy { it.name.lowercase() }
                ?: emptyList()
        } catch (e: SecurityException) {
            Toast.makeText(requireContext(), R.string.folder_picker_unreadable, Toast.LENGTH_SHORT).show()
            emptyList()
        }

        adapter.submitList(subDirs)
        binding.tvEmptyHint.visibility = if (subDirs.isEmpty()) View.VISIBLE else View.GONE
    }

    private fun confirmSelection() {
        onFolderSelected?.invoke(currentDir.absolutePath)
        dismiss()
    }

    // ── Helpers ────────────────────────────────────────────────────────────

    /**
     * Resolves the starting directory: uses [startPath] if it exists and is
     * accessible; otherwise walks up to the first existing ancestor within
     * [ROOT_DIR]; falls back to ROOT_DIR itself.
     */
    private fun resolveStartDir(startPath: String): File {
        var dir = File(startPath)
        while (!dir.exists() || !dir.isDirectory) {
            val parent = dir.parentFile ?: break
            if (!parent.absolutePath.startsWith(ROOT_DIR)) break
            dir = parent
        }
        return if (dir.exists() && dir.isDirectory) dir else File(ROOT_DIR)
    }

    // ── RecyclerView adapter ───────────────────────────────────────────────

    private class FolderAdapter(
        private val onClick: (File) -> Unit,
    ) : ListAdapter<File, FolderAdapter.ViewHolder>(FileDiffCallback()) {

        override fun onCreateViewHolder(parent: ViewGroup, viewType: Int): ViewHolder {
            val binding = ItemFolderBinding.inflate(
                LayoutInflater.from(parent.context), parent, false,
            )
            return ViewHolder(binding)
        }

        override fun onBindViewHolder(holder: ViewHolder, position: Int) =
            holder.bind(getItem(position), onClick)

        class ViewHolder(private val binding: ItemFolderBinding) :
            RecyclerView.ViewHolder(binding.root) {

            fun bind(folder: File, onClick: (File) -> Unit) {
                binding.tvFolderName.text = folder.name
                binding.root.setOnClickListener { onClick(folder) }
            }
        }

        private class FileDiffCallback : DiffUtil.ItemCallback<File>() {
            override fun areItemsTheSame(oldItem: File, newItem: File): Boolean =
                oldItem.absolutePath == newItem.absolutePath

            override fun areContentsTheSame(oldItem: File, newItem: File): Boolean =
                oldItem.absolutePath == newItem.absolutePath
        }
    }
}
