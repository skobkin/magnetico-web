<?php

namespace App\Entity;

use Doctrine\ORM\Mapping as ORM;

/**
 * @ORM\Table(name="files", indexes={
 *     @ORM\Index(name="file_info_hash_index", columns={"torrent_id"})
 * })
 * @ORM\Entity(readOnly=true)
 */
class File
{
    /**
     * @var int
     *
     * @ORM\Column(name="id", type="integer")
     * @ORM\Id
     */
    private $id;

    /**
     * @var Torrent
     *
     * @ORM\ManyToOne(targetEntity="App\Entity\Torrent", inversedBy="files")
     * @ORM\JoinColumn(name="torrent_id")
     */
    private $torrent;

    /**
     * @var int File size in bytes
     *
     * @ORM\Column(name="size", type="integer", nullable=false)
     */
    private $size;

    /**
     * @var string
     *
     * @ORM\Column(name="path", type="text", nullable=false)
     */
    private $path;

    public function getId(): int
    {
        return $this->id;
    }

    public function getTorrent(): Torrent
    {
        return $this->torrent;
    }

    /** Returns file size in bytes */
    public function getSize(): int
    {
        return $this->size;
    }

    /** Returns file path relative to the torrent root directory */
    public function getPath(): string
    {
        return $this->path;
    }
}
