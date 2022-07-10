<?php
declare(strict_types=1);

namespace App\Helper;

use App\Magnetico\Entity\Torrent;
use App\View\Torrent\FileTreeNode;

class BsTreeviewFileTreeBuilder
{
    private const DEFAULT_FILE_ICON = 'fas fa-file';
    private const DEFAULT_DIR_ICON = 'fas fa-folder';

    private FileSizeHumanizer $humanizer;

    public function __construct(FileSizeHumanizer $humanizer)
    {
        $this->humanizer = $humanizer;
    }

    public function buildFileTreeDataArrayFromTorrent(
        Torrent $torrent,
        ?string $fileIcon = self::DEFAULT_FILE_ICON,
        ?string $dirIcon = self::DEFAULT_DIR_ICON
    ): array {
        return $this->buildFileTreeDataArray(
            FileTreeNode::createFromTorrent($torrent),
            $fileIcon,
            $dirIcon
        );
    }

    public function buildFileTreeDataArray(
        FileTreeNode $node,
        ?string $fileIcon = self::DEFAULT_FILE_ICON,
        ?string $dirIcon = self::DEFAULT_DIR_ICON
    ): array {
        $data = [];

        foreach ($node->getChildren() as $name => $child) {
            $element = [
                'text' => '<strong>'.$name.'</strong>',
            ];

            if ($child->isDirectory()) {
                $element['nodes'] = $this->buildFileTreeDataArray($child, $fileIcon, $dirIcon);

                if ($dirIcon) {
                    $element['icon'] = $dirIcon;
                }

                // Adding number of chilren
                $element['text'] .= ' ['.$child->countChildren().']';
            } else {
                if ($fileIcon) {
                    $element['icon'] = $fileIcon;
                }

                // Adding file size.
                $element['text'] .= ' ('.$this->humanizer->humanize($child->getSize()).')';
            }

            $data[] = $element;
        }

        return $data;
    }
}
