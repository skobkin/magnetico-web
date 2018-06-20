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
     * @var int
     *
     * @ORM\ManyToOne(targetEntity="App\Entity\Torrent", inversedBy="files")
     * @ORM\JoinColumn(name="torrent_id")
     */
    private $torrent;

    /**
     * @var int
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

    public function getTorrent(): int
    {
        return $this->torrent;
    }

    public function getSize(): int
    {
        return $this->size;
    }

    public function getPath(): string
    {
        return $this->path;
    }
}
