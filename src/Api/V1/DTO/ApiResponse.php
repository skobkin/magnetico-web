<?php
declare(strict_types=1);

namespace App\Api\V1\DTO;

use Symfony\Component\HttpFoundation\Response;
use Symfony\Component\Serializer\Annotation\Groups;

class ApiResponse
{
    public const STATUS_SUCCESS = 'success';
    public const STATUS_ERROR = 'error';
    public const STATUS_FAIL = 'fail';
    public const STATUS_UNKNOWN = 'unknown';

    #[Groups(['api'])]
    private int $code;

    /** Status text: 'success' (1xx-3xx), 'error' (4xx), 'fail' (5xx) or 'unknown' */
    #[Groups(['api'])]
    private string $status;

    /** Used for 'fail' and 'error') */
    #[Groups(['api'])]
    private ?string $message;

    /** @Response body. In case of 'error' or 'fail' contains cause or exception name. */
    #[Groups(['api'])]
    private string|object|array|null $data;

    public function __construct($data = null, int $code = Response::HTTP_OK, string $message = null, string $status = '')
    {
        $this->data = $data;
        $this->code = $code;
        $this->message = $message;

        if ('' === $status) {
            switch ($code) {
                case ($code >= 100 && $code < 300):
                    $this->status = self::STATUS_SUCCESS;
                    break;
                case ($code >= 400 && $code < 500):
                    $this->status = self::STATUS_ERROR;
                    break;
                case ($code >= 500 && $code < 600):
                    $this->status = self::STATUS_FAIL;
                    break;
                default:
                    $this->status = self::STATUS_UNKNOWN;
            }
        } else {
            $this->status = $status;
        }
    }

    public function getCode(): int
    {
        return $this->code;
    }

    public function getStatus(): string
    {
        return $this->status;
    }

    public function getMessage(): ?string
    {
        return $this->message;
    }

    /** @return array|\object|string|null */
    public function getData()
    {
        return $this->data;
    }
}